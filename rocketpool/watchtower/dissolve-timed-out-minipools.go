package watchtower

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/rocket-pool/rocketpool-go/dao/trustednode"
	"github.com/rocket-pool/rocketpool-go/minipool"
	"github.com/rocket-pool/rocketpool-go/rocketpool"
	"github.com/rocket-pool/rocketpool-go/settings/protocol"
	rptypes "github.com/rocket-pool/rocketpool-go/types"
	"github.com/urfave/cli"
	"golang.org/x/sync/errgroup"

	"github.com/rocket-pool/smartnode/shared/services"
	"github.com/rocket-pool/smartnode/shared/services/config"
	rpgas "github.com/rocket-pool/smartnode/shared/services/gas"
	"github.com/rocket-pool/smartnode/shared/services/wallet"
	"github.com/rocket-pool/smartnode/shared/utils/api"
	"github.com/rocket-pool/smartnode/shared/utils/log"
)

// Settings
const MinipoolStatusBatchSize = 20


// Dissolve timed out minipools task
type dissolveTimedOutMinipools struct {
    c *cli.Context
    log log.ColorLogger
    cfg config.RocketPoolConfig
    w *wallet.Wallet
    ec *ethclient.Client
    rp *rocketpool.RocketPool
    maxFee *big.Int
    maxPriorityFee *big.Int
    gasLimit uint64
}


// Create dissolve timed out minipools task
func newDissolveTimedOutMinipools(c *cli.Context, logger log.ColorLogger) (*dissolveTimedOutMinipools, error) {

    // Get services
    cfg, err := services.GetConfig(c)
    if err != nil { return nil, err }
    w, err := services.GetWallet(c)
    if err != nil { return nil, err }
    ec, err := services.GetEthClient(c)
    if err != nil { return nil, err }
    rp, err := services.GetRocketPool(c)
    if err != nil { return nil, err }

    // Get the user-requested max fee
    maxFee, err := cfg.GetMaxFee()
    if err != nil {
        return nil, fmt.Errorf("Error getting max fee in configuration: %w", err)
    }

    // Get the user-requested max fee
    maxPriorityFee, err := cfg.GetMaxPriorityFee()
    if err != nil {
        return nil, fmt.Errorf("Error getting max priority fee in configuration: %w", err)
    }
    if maxPriorityFee == nil || maxPriorityFee.Uint64() == 0 {
        logger.Println("WARNING: priority fee was missing or 0, setting a default of 2.");
        maxPriorityFee = big.NewInt(2)
    }

    // Get the user-requested gas limit
    gasLimit, err := cfg.GetGasLimit()
    if err != nil {
        return nil, fmt.Errorf("Error getting gas limit in configuration: %w", err)
    }

    // Return task
    return &dissolveTimedOutMinipools{
        c: c,
        log: logger,
        cfg: cfg,
        w: w,
        ec: ec,
        rp: rp,
        maxFee: maxFee,
        maxPriorityFee: maxPriorityFee,
        gasLimit: gasLimit,
    }, nil

}


// Dissolve timed out minipools
func (t *dissolveTimedOutMinipools) run() error {

    // Wait for eth client to sync
    if err := services.WaitEthClientSynced(t.c, true); err != nil {
        return err
    }

    // Get node account
    nodeAccount, err := t.w.GetNodeAccount()
    if err != nil {
        return err
    }

    // Check node trusted status
    nodeTrusted, err := trustednode.GetMemberExists(t.rp, nodeAccount.Address, nil)
    if err != nil {
        return err
    }
    if !nodeTrusted {
        return nil
    }

    // Log
    t.log.Println("Checking for timed out minipools to dissolve...")

    // Get timed out minipools
    minipools, err := t.getTimedOutMinipools()
    if err != nil {
        return err
    }
    if len(minipools) == 0 {
        return nil
    }

    // Log
    t.log.Printlnf("%d minipool(s) have timed out and will be dissolved...", len(minipools))

    // Dissolve minipools
    for _, mp := range minipools {
        if err := t.dissolveMinipool(mp); err != nil {
            t.log.Println(fmt.Errorf("Could not dissolve minipool %s: %w", mp.Address.Hex(), err))
        }
    }

    // Return
    return nil

}


// Get timed out minipools
func (t *dissolveTimedOutMinipools) getTimedOutMinipools() ([]*minipool.Minipool, error) {

    // Data
    var wg1 errgroup.Group
    var addresses []common.Address
    var launchTimeout time.Duration
    var latestEth1Block *types.Header

    // Get minipool addresses
    wg1.Go(func() error {
        var err error
        addresses, err = minipool.GetMinipoolAddresses(t.rp, nil)
        return err
    })

    // Get launch timeout
    wg1.Go(func() error {
        var err error
        launchTimeout, err = protocol.GetMinipoolLaunchTimeout(t.rp, nil)
        return err
    })

    // Get latest block
    wg1.Go(func() error {
        var err error
        latestEth1Block, err = t.ec.HeaderByNumber(context.Background(), nil)
        return err
    })

    // Wait for data
    if err := wg1.Wait(); err != nil {
        return []*minipool.Minipool{}, err
    }

    // Create minipool contracts
    minipools := make([]*minipool.Minipool, len(addresses))
    for mi, address := range addresses {
        mp, err := minipool.NewMinipool(t.rp, address)
        if err != nil {
            return []*minipool.Minipool{}, err
        }
        minipools[mi] = mp
    }

    // Load minipool statuses in batches
    statuses := make([]minipool.StatusDetails, len(minipools))
    for bsi := 0; bsi < len(minipools); bsi += MinipoolStatusBatchSize {

        // Get batch start & end index
        msi := bsi
        mei := bsi + MinipoolStatusBatchSize
        if mei > len(minipools) { mei = len(minipools) }

        // Load statuses
        var wg errgroup.Group
        for mi := msi; mi < mei; mi++ {
            mi := mi
            wg.Go(func() error {
                mp := minipools[mi]
                status, err := mp.GetStatusDetails(nil)
                if err == nil { statuses[mi] = status }
                return err
            })
        }
        if err := wg.Wait(); err != nil {
            return []*minipool.Minipool{}, err
        }

    }

    // Filter minipools by status
    latestBlockTime := time.Unix(int64(latestEth1Block.Time), 0)
    timedOutMinipools := []*minipool.Minipool{}
    for mi, mp := range minipools {
        if statuses[mi].Status == rptypes.Prelaunch && latestBlockTime.Sub(statuses[mi].StatusTime) >= launchTimeout {
            timedOutMinipools = append(timedOutMinipools, mp)
        }
    }

    // Return
    return timedOutMinipools, nil

}


// Dissolve a minipool
func (t *dissolveTimedOutMinipools) dissolveMinipool(mp *minipool.Minipool) error {

    // Log
    t.log.Printlnf("Dissolving minipool %s...", mp.Address.Hex())

    // Get transactor
    opts, err := t.w.GetNodeAccountTransactor()
    if err != nil {
        return err
    }

    // Get the gas limit
    gasInfo, err := mp.EstimateDissolveGas(opts)
    if err != nil {
        return fmt.Errorf("Could not estimate the gas required to dissolve the minipool: %w", err)
    }
    var gas *big.Int 
    if t.gasLimit != 0 {
        gas = new(big.Int).SetUint64(t.gasLimit)
    } else {
        gas = new(big.Int).SetUint64(gasInfo.SafeGasLimit)
    }

    // Get the max fee
    maxFee := t.maxFee
    if maxFee == nil || maxFee.Uint64() == 0 {
        maxFee, err = rpgas.GetHeadlessMaxFeeWei()
        if err != nil {
            return err
        }
    }

    // Print the gas info
    if !api.PrintAndCheckGasInfo(gasInfo, false, 0, t.log, maxFee, t.gasLimit) {
        return nil
    }

    opts.GasFeeCap = maxFee
    opts.GasTipCap = t.maxPriorityFee
    opts.GasLimit = gas.Uint64()

    // Dissolve
    hash, err := mp.Dissolve(opts)
    if err != nil {
        return err
    }

    // Print TX info and wait for it to be mined
    err = api.PrintAndWaitForTransaction(t.cfg, hash, t.rp.Client, t.log)
    if err != nil {
        return err
    }

    // Log
    t.log.Printlnf("Successfully dissolved minipool %s.", mp.Address.Hex())

    // Return
    return nil

}

