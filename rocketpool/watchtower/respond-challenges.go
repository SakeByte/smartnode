package watchtower

import (
	"fmt"
	"math/big"

	"github.com/rocket-pool/rocketpool-go/dao/trustednode"
	"github.com/rocket-pool/rocketpool-go/rocketpool"
	"github.com/urfave/cli"

	"github.com/rocket-pool/smartnode/shared/services"
	"github.com/rocket-pool/smartnode/shared/services/config"
	rpgas "github.com/rocket-pool/smartnode/shared/services/gas"
	"github.com/rocket-pool/smartnode/shared/services/wallet"
	"github.com/rocket-pool/smartnode/shared/utils/api"
	"github.com/rocket-pool/smartnode/shared/utils/log"
)

// Respond to challenges task
type respondChallenges struct {
    c *cli.Context
    log log.ColorLogger
    cfg config.RocketPoolConfig
    w *wallet.Wallet
    rp *rocketpool.RocketPool
    maxFee *big.Int
    maxPriorityFee *big.Int
    gasLimit uint64
}


// Create respond to challenges task
func newRespondChallenges(c *cli.Context, logger log.ColorLogger) (*respondChallenges, error) {

    // Get services
    cfg, err := services.GetConfig(c)
    if err != nil { return nil, err }
    w, err := services.GetWallet(c)
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
    return &respondChallenges{
        c: c,
        log: logger,
        cfg: cfg,
        w: w,
        rp: rp,
        maxFee: maxFee,
        maxPriorityFee: maxPriorityFee,
        gasLimit: gasLimit,
    }, nil

}


// Respond to challenges
func (t *respondChallenges) run() error {

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
    t.log.Println("Checking for challenges to respond to...")

    // Check for active challenges
    isChallenged, err := trustednode.GetMemberIsChallenged(t.rp, nodeAccount.Address, nil)
    if err != nil {
        return err
    }
    if !isChallenged {
        return nil
    }

    // Log
    t.log.Printlnf("Node %s has an active challenge against it, responding...", nodeAccount.Address.Hex())

    // Get transactor
    opts, err := t.w.GetNodeAccountTransactor()
    if err != nil {
        return err
    }

    // Get the gas limit
    gasInfo, err := trustednode.EstimateDecideChallengeGas(t.rp, nodeAccount.Address, opts)
    if err != nil {
        return fmt.Errorf("Could not estimate the gas required to respond to the challenge: %w", err)
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

    // Respond to challenge
    hash, err := trustednode.DecideChallenge(t.rp, nodeAccount.Address, opts)
    if err != nil {
        return err
    }

    // Print TX info and wait for it to be mined
    err = api.PrintAndWaitForTransaction(t.cfg, hash, t.rp.Client, t.log)
    if err != nil {
        return err
    }

    // Log & return
    t.log.Printlnf("Successfully responded to challenge against node %s.", nodeAccount.Address.Hex())
    return nil

}

