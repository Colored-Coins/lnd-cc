package lnwallet

import (
	"errors"
	"fmt"
	"sync"

	"github.com/lightningnetwork/lnd/lndcc"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

// ErrNotMine is an error denoting that a WalletController instance is unable
// to spend a specifid output.
var ErrNotMine = errors.New("the passed output doesn't belong to the wallet")

// AddressType is a enum-like type which denotes the possible address types
// WalletController supports.
type AddressType uint8

const (
	// WitnessPubKey represents a p2wkh address.
	WitnessPubKey AddressType = iota

	// NestedWitnessPubKey represents a p2sh output which is itself a
	// nested p2wkh output.
	NestedWitnessPubKey

	// PublicKey represents a regular p2pkh output.
	PubKeyHash
)

// Utxo is an unspent output denoted by its outpoint, and output value of the
// original output.
type Utxo struct {
	Value     btcutil.Amount
	ColorData *lndcc.TxoData
	wire.OutPoint
}

// WalletController defines an abstract interface for controlling a local Pure
// Go wallet, a local or remote wallet via an RPC mechanism, or possibly even
// a daemon assisted hardware wallet. This interface serves the purpose of
// allowing LightningWallet to be seamlessly compatible with several wallets
// such as: uspv, btcwallet, Bitcoin Core, Electrum, etc. This interface then
// serves as a "base wallet", with Lightning Network awareness taking place at
// a "higher" level of abstraction. Essentially, an overlay wallet.
// Implementors of this interface must closely adhere to the documented
// behavior of all interface methods in order to ensure identical behavior
// across all concrete implementations.
type WalletController interface {
	// FetchInputInfo queries for the WalletController's knowledge of the
	// passed outpoint. If the base wallet determines this output is under
	// its control, then the original txout should be returned. Otherwise,
	// a non-nil error value of ErrNotMine should be returned instead.
	FetchInputInfo(prevOut *wire.OutPoint) (*wire.TxOut, error)

	// ConfirmedBalance returns the sum of all the wallet's unspent outputs
	// that have at least confs confirmations. If confs is set to zero,
	// then all unspent outputs, including those currently in the mempool
	// will be included in the final sum.
	ConfirmedBalance(confs int32, witness bool) (btcutil.Amount, error)

	// NewAddress returns the next external or internal address for the
	// wallet dicatated by the value of the `change` paramter. If change is
	// true, then an internal address should be used, otherwise an external
	// address should be returned. The type of address returned is dictated
	// by the wallet's capabilities, and may be of type: p2sh, p2pkh,
	// p2wkh, p2wsh, etc.
	NewAddress(addrType AddressType, change bool) (btcutil.Address, error)

	// GetPrivKey retrives the underlying private key associated with the
	// passed address. If the wallet is unable to locate this private key
	// due to the address not being under control of the wallet, then an
	// error should be returned.
	// TODO(roasbeef): should instead take tadge's derivation scheme in
	GetPrivKey(a btcutil.Address) (*btcec.PrivateKey, error)

	// NewRawKey returns a raw private key controlled by the wallet. These
	// keys are used for the 2-of-2 multi-sig outputs for funding
	// transactions, as well as the pub key used for commitment transactions.
	// TODO(roasbeef): may be scrapped, see above TODO
	NewRawKey() (*btcec.PublicKey, error)

	// FetchRootKey returns a root key which will be used by the
	// LightningWallet to deterministically generate secrets. The private
	// key returned by this method should remain constant in-between
	// WalletController restarts.
	FetchRootKey() (*btcec.PrivateKey, error)

	// SendOutputs funds, signs, and broadcasts a Bitcoin transaction
	// paying out to the specified outputs. In the case the wallet has
	// insufficient funds, or the outputs are non-standard, and error
	// should be returned.
	SendOutputs(outputs []*wire.TxOut) (*wire.ShaHash, error)

	// ListUnspentWitness returns all unspent outputs which are version 0
	// witness programs. The 'confirms' parameter indicates the minimum
	// number of confirmations an output needs in order to be returned by
	// this method. Passing -1 as 'confirms' indicates that even
	// unconfirmed outputs should be returned.
	ListUnspentWitness(confirms int32) ([]*Utxo, error)

	// LockOutpoint marks an outpoint as locked meaning it will no longer
	// be deemed as eligible for coin selection. Locking outputs are
	// utilized in order to avoid race conditions when selecting inputs for
	// usage when funding a channel.
	LockOutpoint(o wire.OutPoint)

	// UnlockOutpoint unlocks an previously locked output, marking it
	// eligible for coin seleciton.
	UnlockOutpoint(o wire.OutPoint)

	// PublishTransaction performs cursory validation (dust checks, etc),
	// then finally broadcasts the passed transaction to the Bitcoin network.
	PublishTransaction(tx *wire.MsgTx) error

	// Start initializes the wallet, making any neccessary connections,
	// starting up required goroutines etc.
	Start() error

	// Stop signals the wallet for shutdown. Shutdown may entail closing
	// any active sockets, database handles, stopping goroutines, etc.
	Stop() error
}

// BlockChainIO is a dedicated source which will be used to obtain queries
// related to the current state of the blockchain. The data returned by each of
// the defined methods within this interface should always return the most up
// to date data possible.
//
// TODO(roasbeef): move to diff package perhaps?
// TODO(roasbeef): move publish txn here?
type BlockChainIO interface {
	// GetCurrentHeight returns the current height of the valid most-work
	// chain the implementation is aware of.
	GetCurrentHeight() (int32, error)

	// GetTxOut returns the original output referenced by the passed
	// outpoint.
	GetUtxo(txid *wire.ShaHash, index uint32) (*wire.TxOut, error)

	// GetTransaction returns the full transaction identified by the passed
	// transaction ID.
	GetTransaction(txid *wire.ShaHash) (*wire.MsgTx, error)
}

// SignDescriptor houses the necessary information required to succesfully sign
// a given output. This struct is used by the Signer interface in order to gain
// access to critial data needed to generate a valid signature.
type SignDescriptor struct {
	// Pubkey is the public key to which the signature should be generated
	// over. The Signer should then generate a signature with the private
	// key corresponding to this public key.
	PubKey *btcec.PublicKey

	// RedeemScript is the full script required to properly redeem the
	// output. This field will only be populated if a p2wsh or a p2sh
	// output is being signed.
	RedeemScript []byte

	// Output is the target output which should be signed. The PkScript and
	// Value fields within the output should be properly populated,
	// otherwise an invalid signature may be generated.
	Output *wire.TxOut

	// HashType is the target sighash type that should be used when
	// generating the final sighash, and signature.
	HashType txscript.SigHashType

	// SigHashes is the pre-computed sighash midstate to be used when
	// generating the final sighash for signing.
	SigHashes *txscript.TxSigHashes

	// InputIndex is the target input within the transaction that should be
	// signed.
	InputIndex int
}

// Signer represents an abstract object capable of generating raw signatures as
// well as full complete input scripts given a valid SignDescriptor and
// transaction. This interface fully abstracts away signing paving the way for
// Signer implementations such as hardware wallets, hardware tokens, HSM's, or
// simply a regular wallet.
type Signer interface {
	// SignOutputRaw generates a signature for the passed transaction
	// according to the data within the passed SignDescriptor.
	//
	// NOTE: The resulting signature should be void of a sighash byte.
	SignOutputRaw(tx *wire.MsgTx, signDesc *SignDescriptor) ([]byte, error)

	// ComputeInputScript generates a complete InputIndex for the passed
	// transaction with the signature as defined within the passed
	// SignDescriptor. This method should be capable of generating the
	// proper input script for both regular p2wkh output and p2wkh outputs
	// nested within a regualr p2sh output.
	ComputeInputScript(tx *wire.MsgTx, signDesc *SignDescriptor) (*InputScript, error)
}

// WalletDriver represents a "driver" for a particular concrete
// WalletController implementation. A driver is indentified by a globally
// unique string identifier along with a 'New()' method which is responsible
// for initializing a particular WalletController concrete implementation.
type WalletDriver struct {
	// WalletType is a string which uniquely identifes the WalletController
	// that this driver, drives.
	WalletType string

	// New creates a new instance of a concrete WalletController
	// implementation given a variadic set up arguments. The function takes
	// a varidaic number of interface paramters in order to provide
	// initialization flexibility, thereby accomodating several potential
	// WalletController implementations.
	New func(args ...interface{}) (WalletController, error)
}

var (
	wallets     = make(map[string]*WalletDriver)
	registerMtx sync.Mutex
)

// RegisteredWallets returns a slice of all currently registered notifiers.
//
// NOTE: This function is safe for concurrent access.
func RegisteredWallets() []*WalletDriver {
	registerMtx.Lock()
	defer registerMtx.Unlock()

	registeredWallets := make([]*WalletDriver, 0, len(wallets))
	for _, wallet := range wallets {
		registeredWallets = append(registeredWallets, wallet)
	}

	return registeredWallets
}

// RegisterWallet registers a WalletDriver which is capable of driving a
// concrete WalletController interface. In the case that this driver has
// already been registered, an error is returned.
//
// NOTE: This function is safe for concurrent access.
func RegisterWallet(driver *WalletDriver) error {
	registerMtx.Lock()
	defer registerMtx.Unlock()

	if _, ok := wallets[driver.WalletType]; ok {
		return fmt.Errorf("wallet already registered")
	}

	wallets[driver.WalletType] = driver

	return nil
}

// SupportedWallets returns a slice of strings that represents the walelt
// drivers that have been registered and are therefore supported.
//
// NOTE: This function is safe for concurrent access.
func SupportedWallets() []string {
	registerMtx.Lock()
	defer registerMtx.Unlock()

	supportedWallets := make([]string, 0, len(wallets))
	for walletName := range wallets {
		supportedWallets = append(supportedWallets, walletName)
	}

	return supportedWallets
}
