package wallet

import (
    "bytes"
    "errors"
    "fmt"
    "sync"

    rptypes "github.com/rocket-pool/rocketpool-go/types"
    eth2types "github.com/wealdtech/go-eth2-types/v2"
    eth2utilv1_5 "github.com/rocket-pool/go-eth2-util-hkdf3"
    eth2utilv1_6 "github.com/wealdtech/go-eth2-util"
)


// Config
const (
    ValidatorKeyPath = "m/12381/3600/%d/0/0"
    MaxValidatorKeyRecoverAttempts = 100
)


// Get the number of validator keys recorded in the wallet
func (w *Wallet) GetValidatorKeyCount() (uint, error) {

    // Check wallet is initialized
    if !w.IsInitialized() {
        return 0, errors.New("Wallet is not initialized")
    }

    // Return validator key count
    return w.ws.NextAccount, nil

}


// Get a validator key by index
func (w *Wallet) GetValidatorKeyAt(index uint) (*eth2types.BLSPrivateKey, error) {

    // Check wallet is initialized
    if !w.IsInitialized() {
        return nil, errors.New("Wallet is not initialized")
    }

    // Return validator key
    key, _, err := w.getValidatorPrivateKey(index, false)
    return key, err

}


// Get a validator key by public key
func (w *Wallet) GetValidatorKeyByPubkey(pubkey rptypes.ValidatorPubkey) (*eth2types.BLSPrivateKey, error) {

    // Check wallet is initialized
    if !w.IsInitialized() {
        return nil, errors.New("Wallet is not initialized")
    }

    // Get pubkey hex string
    pubkeyHex := pubkey.Hex()

    // Check for cached validator key index
    if index, ok := w.validatorKeyIndices[pubkeyHex]; ok {

        // Try hkdfv4
        if key, _, err := w.getValidatorPrivateKey(index, false); err != nil {
            return nil, err
        } else if bytes.Equal(pubkey.Bytes(), key.PublicKey().Marshal()) {
            return key, nil
        }

        // Try hkdfv3
        if key, _, err := w.getValidatorPrivateKey(index, true); err != nil {
            return nil, err
        } else if bytes.Equal(pubkey.Bytes(), key.PublicKey().Marshal()) {
            return key, nil
        }

        // No match
        return nil, fmt.Errorf("Validator %s key not found", pubkey.Hex())

    }

    // Find matching validator key
    var index uint
    var validatorKey *eth2types.BLSPrivateKey
    for index = 0; index < w.ws.NextAccount; index++ {

        // Try hkdfv4
        if key, _, err := w.getValidatorPrivateKey(index, false); err != nil {
            return nil, err
        } else if bytes.Equal(pubkey.Bytes(), key.PublicKey().Marshal()) {
            validatorKey = key
            break
        }

        // Try hkdfv3
        if key, _, err := w.getValidatorPrivateKey(index, true); err != nil {
            return nil, err
        } else if bytes.Equal(pubkey.Bytes(), key.PublicKey().Marshal()) {
            validatorKey = key
            break
        }

    }

    // Check validator key
    if validatorKey == nil {
        return nil, fmt.Errorf("Validator %s key not found", pubkeyHex)
    }

    // Cache validator key index
    w.validatorKeyIndices[pubkeyHex] = index

    // Return
    return validatorKey, nil

}


// Create a new validator key
func (w *Wallet) CreateValidatorKey() (*eth2types.BLSPrivateKey, error) {

    // Check wallet is initialized
    if !w.IsInitialized() {
        return nil, errors.New("Wallet is not initialized")
    }

    // Get & increment account index
    index := w.ws.NextAccount
    w.ws.NextAccount++

    // Get validator key
    key, path, err := w.getValidatorPrivateKey(index, false)
    if err != nil {
        return nil, err
    }

    // Update keystores
    for name, ks := range w.keystores {
        if err := ks.StoreValidatorKey(key, path); err != nil {
            return nil, fmt.Errorf("Could not store %s validator key: %w", name, err)
        }
    }

    // Return validator key
    return key, nil

}


// Recover a validator key by public key
func (w *Wallet) RecoverValidatorKey(pubkey rptypes.ValidatorPubkey) error {

    // Check wallet is initialized
    if !w.IsInitialized() {
        return errors.New("Wallet is not initialized")
    }

    // Find matching validator key
    var index uint
    var validatorKey *eth2types.BLSPrivateKey
    var derivationPath string
    for index = 0; index < w.ws.NextAccount + MaxValidatorKeyRecoverAttempts; index++ {

        // Try hkdfv4
        if key, path, err := w.getValidatorPrivateKey(index, false); err != nil {
            return err
        } else if bytes.Equal(pubkey.Bytes(), key.PublicKey().Marshal()) {
            validatorKey = key
            derivationPath = path
            break
        }

        // Try hkdfv3
        if key, path, err := w.getValidatorPrivateKey(index, true); err != nil {
            return err
        } else if bytes.Equal(pubkey.Bytes(), key.PublicKey().Marshal()) {
            validatorKey = key
            derivationPath = path
            break
        }

    }

    // Check validator key
    if validatorKey == nil {
        return fmt.Errorf("Validator %s key not found", pubkey.Hex())
    }

    // Update account index
    nextIndex := index + 1
    if nextIndex > w.ws.NextAccount {
        w.ws.NextAccount = nextIndex
    }

    // Update keystores
    for name, ks := range w.keystores {
        if err := ks.StoreValidatorKey(validatorKey, derivationPath); err != nil {
            return fmt.Errorf("Could not store %s validator key: %w", name, err)
        }
    }

    // Return
    return nil

}


// Get a validator private key by index
func (w *Wallet) getValidatorPrivateKey(index uint, hkdfv3 bool) (*eth2types.BLSPrivateKey, string, error) {

    // Get derivation path
    derivationPath := fmt.Sprintf(ValidatorKeyPath, index)

    // Check for cached validator key
    if hkdfv3 {
        if validatorKey, ok := w.validatorKeys1[index]; ok {
            return validatorKey, derivationPath, nil
        }
    } else {
        if validatorKey, ok := w.validatorKeys2[index]; ok {
            return validatorKey, derivationPath, nil
        }
    }

    // Initialize BLS support
    initializeBLS()

    // Get private key
    var privateKey *eth2types.BLSPrivateKey
    var err error
    if hkdfv3 {
        privateKey, err = eth2utilv1_5.PrivateKeyFromSeedAndPath(w.seed, derivationPath)
    } else {
        privateKey, err = eth2utilv1_6.PrivateKeyFromSeedAndPath(w.seed, derivationPath)
    }
    if err != nil {
        return nil, "", fmt.Errorf("Could not get validator %d private key: %w", index, err)
    }

    // Cache validator key
    if hkdfv3 {
        w.validatorKeys1[index] = privateKey
    } else {
        w.validatorKeys2[index] = privateKey
    }

    // Return
    return privateKey, derivationPath, nil

}


// Initialize BLS support
var initBLS sync.Once
func initializeBLS() {
    initBLS.Do(func() {
        eth2types.InitBLS()
    })
}

