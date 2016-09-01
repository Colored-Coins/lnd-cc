package lnwallet

import (
	"bytes"
	"container/list"
	"fmt"
	"sync"
	"log"

	"github.com/btcsuite/fastsha256"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"github.com/roasbeef/btcutil/txsort"
)

var zeroHash wire.ShaHash

var (
	ErrChanClosing = fmt.Errorf("channel is being closed, operation disallowed")
	ErrNoWindow    = fmt.Errorf("unable to sign new commitment, the current" +
		" revocation window is exhausted")
)

const (
	// MaxPendingPayments is the max number of pending HTLC's permitted on
	// a channel.
	// TODO(roasbeef): make not random value + enforce
	//  * should be tuned to account for max tx "cost"
	MaxPendingPayments = 100

	// InitialRevocationWindow is the number of unrevoked commitment
	// transactions allowed within the commitment chain. This value allows
	// a greater degree of desynchronization by allowing either parties to
	// extend the other's commitment chain non-interactively, and also
	// serves as a flow control mechanism to a degree.
	InitialRevocationWindow = 4
)

// channelState is an enum like type which represents the current state of a
// particular channel.
// TODO(roasbeef): actually update state
type channelState uint8

const (
	// channelPending indicates this channel is still going through the
	// funding workflow, and isn't yet open.
	channelPending channelState = iota

	// channelOpen represents an open, active channel capable of
	// sending/receiving HTLCs.
	channelOpen

	// channelClosing represents a channel which is in the process of being
	// closed.
	channelClosing

	// channelClosed represents a channel which has been fully closed. Note
	// that before a channel can be closed, ALL pending HTLC's must be
	// settled/removed.
	channelClosed

	// channelDispute indicates that an un-cooperative closure has been
	// detected within the channel.
	channelDispute

	// channelPendingPayment indicates that there a currently outstanding
	// HTLC's within the channel.
	channelPendingPayment
)

// PaymentHash represents the sha256 of a random value. This hash is used to
// uniquely track incoming/outgoing payments within this channel, as well as
// payments requested by the wallet/daemon.
type PaymentHash [32]byte

// UpdateType is the exact type of an entry within the shared HTLC log.
type updateType uint8

const (
	Add updateType = iota
	Timeout
	Settle
)

// PaymentDescriptor represents a commitment state update which either adds,
// settles, or removes an HTLC. PaymentDescriptors encapsulate all necessary
// meta-data w.r.t to an HTLC, and additional data pairing a settle message to
// the original added HTLC.
type PaymentDescriptor struct {
	// TODO(roasbeef): LogEntry interface??
	sync.RWMutex

	// RHash is the payment hash for this HTLC. The HTLC can be settled iff
	// the preimage to this hash is presented.
	RHash PaymentHash

	// Timeout is the absolute timeout in blocks, afterwhich this HTLC
	// expires.
	Timeout uint32

	// Amount is the HTLC amount in satoshis.
	Amount btcutil.Amount

	// Index is the log entry number that his HTLC update has within the
	// log. Depending on if IsIncoming is true, this is either an entry the
	// remote party added, or one that we added locally.
	Index uint32

	// ParentIndex is the index of the log entry that this HTLC update
	// settles or times out. If IsIncoming is false, then this refers to an
	// index within our local log, otherwise this refers to an entry int he
	// remote peer's log.
	ParentIndex uint32

	// Payload is an opaque blob which is used to complete multi-hop routing.
	Payload []byte

	// Type denotes the exact type of the PaymentDescriptor. In the case of
	// a Timeout, or Settle type, then the Parent field will point into the
	// log to the HTLC being modified.
	EntryType updateType

	// addCommitHeight[Remote|Local] encodes the height of the commitment
	// which included this HTLC on either the remote or local commitment
	// chain. This value is used to determine when an HTLC is fully
	// "locked-in".
	addCommitHeightRemote uint64
	addCommitHeightLocal  uint64

	// removeCommitHeight[Remote|Local] encodes the height of the
	//commitment which removed the parent pointer of this PaymentDescriptor
	//either due to a timeout or a settle. Once both these heights are
	//above the tail of both chains, the log entries can safely be removed.
	removeCommitHeightRemote uint64
	removeCommitHeightLocal  uint64

	// isForwarded denotes if an incoming HTLC has been forwarded to any
	// possible upstream peers in the route.
	isForwarded bool
	settled     bool
}

// commitment represents a commitment to a new state within an active channel.
// New commitments can be initiated by either side. Commitments are ordered
// into a commitment chain, with one existing for both parties. Each side can
// independatly extend the other side's commitment chain, up to a certain
// "revocation window", which once reached, dissallows new commitments until
// the local nodes receives the revocation for the remote node's chain tail.
type commitment struct {
	// height represents the commitment height of this commitment, or the
	// update number of this commitment.
	height uint64

	// [our|their]MessageIndex are indexes into the HTLC log, up to which
	// this commitment transaction includes. These indexes allow both sides
	// to independantly, and concurrent send create new commitments. Each
	// new commitment sent to the remote party includes an index in the
	// shared log which details which of their updates we're including in
	// this new commitment.
	// TODO(roasbeef): also make uint64?
	ourMessageIndex   uint32
	theirMessageIndex uint32

	// txn is the commitment transaction generated by including any HTLC
	// updates whose index are below the two indexes listed above. If this
	// commitment is being added to the remote chain, then this txn is
	// their version of the commitment transactions. If the local commit
	// chain is being modified, the opposite is true.
	txn *wire.MsgTx

	// sig is a signature for the above commitment transaction.
	sig []byte

	// [our|their]Balance represents the settled balances at this point
	// within the commitment chain. This balance is computed by properly
	// evaluating all the add/remove/settle log entries before the listed
	// indexes.
	ourBalance   btcutil.Amount
	theirBalance btcutil.Amount
}

// commitmentChain represents a chain of unrevoked commitments. The tail of the
// chain is the latest fully signed, yet unrevoked commitment. Two chains are
// tracked, one for the local node, and another for the remote node. New
// commitmetns we create locally extend the remote node's chain, and vice
// versa. Commitment chains are allowed to grow to a bounded length, after
// which the tail needs to be "dropped" before new commitments can be received.
// The tail is "dropped" when the owner of the chain sends a revocation for the
// previous tail.
type commitmentChain struct {
	// commitments is a linked list of commitments to new states. New
	// commitments are added to the end of the chain with increase height.
	// Once a commitment transaction is revoked, the tail is incremented,
	// freeing up the revocation window for new commitments.
	commitments *list.List

	// startingHeight is the starting height of this commitment chain on a
	// session basis.
	startingHeight uint64
}

// newCommitmentChain creates a new commitment chain from an initial height.
func newCommitmentChain(initialHeight uint64) *commitmentChain {
	return &commitmentChain{
		commitments:    list.New(),
		startingHeight: initialHeight,
	}
}

// addCommitment extends the commitment chain by a single commitment. This
// added commitment represents a state update propsed by either party. Once the
// commitment prior to this commitment is revoked, the commitment becomes the
// new defacto state within the channel.
func (s *commitmentChain) addCommitment(c *commitment) {
	s.commitments.PushBack(c)
}

// advanceTail reduces the length of the commitment chain by one. The tail of
// the chain should be advanced once a revocation for the lowest unrevoked
// commitment in the chain is received.
func (s *commitmentChain) advanceTail() {
	s.commitments.Remove(s.commitments.Front())
}

// tip returns the latest commitment added to the chain.
func (s *commitmentChain) tip() *commitment {
	return s.commitments.Back().Value.(*commitment)
}

// tail returns the lowest unrevoked commitment transaction in the chain.
func (s *commitmentChain) tail() *commitment {
	return s.commitments.Front().Value.(*commitment)
}

// LightningChannel implements the state machine which corresponds to the
// current commitment protocol wire spec. The state machine implemented allows
// for asynchronous fully desynchronized, batched+pipelined updates to
// commitment transactions allowing for a high degree of non-blocking
// bi-directional payment throughput.
//
// In order to allow updates to be fully non-blocking, either side is able to
// create multiple new commitment states up to a pre-determined window size.
// This window size is encoded within InitialRevocationWindow. Before the start
// of a session, both side should send out revocation messages with nil
// preimages in order to populate their revocation window for the remote party.
// Ths method .ExtendRevocationWindow() is used to extend the revocation window
// by a single revocation.
//
// The state machine has for main methods:
//  * .SignNextCommitment()
//    * Called one one wishes to sign the next commitment, either initiating a
//      new state update, or responding to a received commitment.
/// * .ReceiveNewCommitment()
//    * Called upon receipt of a new commitment from the remote party. If the
//      new commitment is valid, then a revocation should immediately be
//      generated and sent.
//  * .RevokeCurrentCommitment()
//    * Revokes the current commitment. Should be called directly after
//      receiving a new commitment.
//  * .ReceiveRevocation()
//   * Processes a revocation from the remote party. If successful creates a
//     new defacto broadcastable state.
//
// See the individual comments within the above methods for further details.
type LightningChannel struct {
	// TODO(roasbeef): temporarily replace wallet with either goroutine or
	// BlockChainIO interface? Later once all signing is done through
	// wallet can add back.
	lnwallet *LightningWallet

	channelEvents chainntnfs.ChainNotifier

	sync.RWMutex

	ourLogCounter   uint32
	theirLogCounter uint32

	status channelState

	// currentHeight is the current height of our local commitment chain.
	// This is also the same as the number of updates to the channel we've
	// accepted.
	currentHeight uint64

	// revocationWindowEdge is the edge of the current revocation window.
	// New revocations for prior states created by this channel extend the
	// edge of this revocation window. The existence of a revocation window
	// allows the remote party to initiate new state updates independantly
	// until the window is exhausted.
	revocationWindowEdge uint64

	// usedRevocations is a slice of revocations given to us by the remote
	// party that we've used. This slice is extended each time we create a
	// new commitment. The front of the slice is popped off once we receive
	// a revocation for a prior state. This head element then becomes the
	// next set of keys/hashes we expect to be revoked.
	usedRevocations []*lnwire.CommitRevocation

	// revocationWindow is a window of revocations sent to use by the
	// remote party, allowing us to create new commitment transactions
	// until depleated. The revocations don't contain a valid pre-iamge,
	// only an additional key/hash allowing us to create a new commitment
	// transaction for the remote node that they are able to revoke. If
	// this slice is empty, then we cannot make any new updates to their
	// commitment chain.
	revocationWindow []*lnwire.CommitRevocation

	// remoteCommitChain is the remote node's commitment chain. Any new
	// commitments we initiate are added to the tip of this chain.
	remoteCommitChain *commitmentChain

	// localCommitChain is our local commitment chain. Any new commitments
	// received are added to the tip of this chain. The tail (or lowest
	// height) in this chain is our current accepted state, which we are
	// able to broadcast safely.
	localCommitChain *commitmentChain

	// stateMtx protects concurrent access to the state struct.
	stateMtx     sync.RWMutex
	channelState *channeldb.OpenChannel

	// stateUpdateLog is a (mostly) append-only log storing all the HTLC
	// updates to this channel. The log is walked backwards as HTLC updates
	// are applied in order to re-construct a commitment transaction from a
	// commitment. The log is compacted once a revocation is received.
	ourUpdateLog   *list.List
	theirUpdateLog *list.List

	// logIndex is an index into the above log. This index is used to
	// remove Add state updates, once a timeout/settle is received.
	ourLogIndex   map[uint32]*list.Element
	theirLogIndex map[uint32]*list.Element

	fundingTxIn  *wire.TxIn
	fundingP2WSH []byte

	channelDB *channeldb.DB

	started  int32
	shutdown int32

	quit chan struct{}
	wg   sync.WaitGroup
}

// NewLightningChannel creates a new, active payment channel given an
// implementation of the wallet controller, chain notifier, channel database,
// and the current settled channel state. Throughout state transitions, then
// channel will automatically persist pertinent state to the database in an
// efficient manner.
func NewLightningChannel(wallet *LightningWallet, events chainntnfs.ChainNotifier,
	chanDB *channeldb.DB, state *channeldb.OpenChannel) (*LightningChannel, error) {

	// TODO(roasbeef): remove events+wallet
	lc := &LightningChannel{
		lnwallet:             wallet,
		channelEvents:        events,
		currentHeight:        state.NumUpdates,
		remoteCommitChain:    newCommitmentChain(state.NumUpdates),
		localCommitChain:     newCommitmentChain(state.NumUpdates),
		channelState:         state,
		revocationWindowEdge: state.NumUpdates,
		ourUpdateLog:         list.New(),
		theirUpdateLog:       list.New(),
		ourLogIndex:          make(map[uint32]*list.Element),
		theirLogIndex:        make(map[uint32]*list.Element),
		channelDB:            chanDB,
	}

	// Initialize both of our chains the current un-revoked commitment for
	// each side.
	initialCommitment := &commitment{
		height:            lc.currentHeight,
		ourBalance:        state.OurBalance,
		ourMessageIndex:   0,
		theirBalance:      state.TheirBalance,
		theirMessageIndex: 0,
	}
	lc.localCommitChain.addCommitment(initialCommitment)
	lc.remoteCommitChain.addCommitment(initialCommitment)

	// TODO(roasbeef): do a NotifySpent for the funding input, and
	// NotifyReceived for all commitment outputs.

	fundingPkScript, err := witnessScriptHash(state.FundingRedeemScript)
	if err != nil {
		return nil, err
	}
	lc.fundingTxIn = wire.NewTxIn(state.FundingOutpoint, nil, nil)
	lc.fundingP2WSH = fundingPkScript

	return lc, nil
}

type htlcView struct {
	ourUpdates   []*PaymentDescriptor
	theirUpdates []*PaymentDescriptor
}

// fetchHTLCView returns all the candidate HTLC updates which should be
// considered for inclusion within a commitment based on the passed HTLC log
// indexes.
func (lc *LightningChannel) fetchHTLCView(theirLogIndex, ourLogIndex uint32) *htlcView {
	var ourHTLCs []*PaymentDescriptor
	for e := lc.ourUpdateLog.Front(); e != nil; e = e.Next() {
		htlc := e.Value.(*PaymentDescriptor)

		// This HTLC is active from this point-of-view iff the log
		// index of the state update is below the specified index in
		// our update log.
		if htlc.Index < ourLogIndex {
			ourHTLCs = append(ourHTLCs, htlc)
		}
	}

	var theirHTLCs []*PaymentDescriptor
	for e := lc.theirUpdateLog.Front(); e != nil; e = e.Next() {
		htlc := e.Value.(*PaymentDescriptor)

		// If this is an incoming HTLC, then it is only active from
		// this point-of-view if the index of the HTLC addition in
		// their log is below the specified view index.
		if htlc.Index < theirLogIndex {
			theirHTLCs = append(theirHTLCs, htlc)
		}
	}

	return &htlcView{
		ourUpdates:   ourHTLCs,
		theirUpdates: theirHTLCs,
	}
}

// fetchCommitmentView returns a populated commitment which expresses the state
// of the channel from the point of view of a local or remote chain, evaluating
// the HTLC log up to the passed indexes. This function is used to construct
// both local and remote commitment transactions in order to sign or verify new
// commitment updates. A fully populated commitment is returned which reflects
// the proper balances for both sides at this point in the commitment chain.
func (lc *LightningChannel) fetchCommitmentView(remoteChain bool,
	ourLogIndex, theirLogIndex uint32, revocationKey *btcec.PublicKey,
	revocationHash [32]byte) (*commitment, error) {

	var commitChain *commitmentChain
	if remoteChain {
		commitChain = lc.remoteCommitChain
	} else {
		commitChain = lc.localCommitChain
	}

	// TODO(roasbeef): don't assume view is always fetched from tip?
	var ourBalance, theirBalance btcutil.Amount
	if commitChain.tip() == nil {
		ourBalance = lc.channelState.OurBalance
		theirBalance = lc.channelState.TheirBalance
	} else {
		ourBalance = commitChain.tip().ourBalance
		theirBalance = commitChain.tip().theirBalance
	}

	nextHeight := commitChain.tip().height + 1

	// Run through all the HTLC's that will be covered by this transaction
	// in order to update their commitment addition height, and to adjust
	// the balances on the commitment transaction accordingly.
	// TODO(roasbeef): error if log empty?
	htlcView := lc.fetchHTLCView(theirLogIndex, ourLogIndex)
	filteredHTLCView := lc.evaluateHTLCView(htlcView, &ourBalance, &theirBalance,
		nextHeight, remoteChain)

	var selfKey *btcec.PublicKey
	var remoteKey *btcec.PublicKey
	var delay uint32
	var delayBalance, p2wkhBalance btcutil.Amount
	if remoteChain {
		selfKey = lc.channelState.TheirCommitKey
		remoteKey = lc.channelState.OurCommitKey.PubKey()
		delay = lc.channelState.RemoteCsvDelay
		delayBalance = theirBalance
		p2wkhBalance = ourBalance
	} else {
		selfKey = lc.channelState.OurCommitKey.PubKey()
		remoteKey = lc.channelState.TheirCommitKey
		delay = lc.channelState.LocalCsvDelay
		delayBalance = ourBalance
		p2wkhBalance = theirBalance
	}

	// Generate a new commitment transaction with all the latest
	// unsettled/un-timed out HTLC's.
	ourCommitTx := !remoteChain
	commitTx, err := createCommitTx(lc.fundingTxIn, selfKey, remoteKey,
		revocationKey, delay, delayBalance, p2wkhBalance)
	if err != nil {
		return nil, err
	}
	for _, htlc := range filteredHTLCView.ourUpdates {
		if err := lc.addHTLC(commitTx, ourCommitTx, htlc,
			revocationHash, delay, false); err != nil {
			return nil, err
		}
	}
	for _, htlc := range filteredHTLCView.theirUpdates {
		if err := lc.addHTLC(commitTx, ourCommitTx, htlc,
			revocationHash, delay, true); err != nil {
			return nil, err
		}
	}

	// Sort the transactions according to the agreed upon cannonical
	// ordering. This lets us skip sending the entire transaction over,
	// instead we'll just send signatures.
	txsort.InPlaceSort(commitTx)

	commitTx, err = ColorifyTx(commitTx, false)
	if err != nil { return nil, err }

	return &commitment{
		txn:               commitTx,
		height:            nextHeight,
		ourBalance:        ourBalance,
		ourMessageIndex:   ourLogIndex,
		theirMessageIndex: theirLogIndex,
		theirBalance:      theirBalance,
	}, nil
}

// evaluateHTLCView processes all update entries in both HTLC update logs,
// producing a final view which is the result of properly applying all adds,
// settles, and timeouts found in both logs. The resulting view returned
// reflects the current state of htlc's within the remote or local commitment
// chain.
func (lc *LightningChannel) evaluateHTLCView(view *htlcView, ourBalance,
	theirBalance *btcutil.Amount, nextHeight uint64, remoteChain bool) *htlcView {

	newView := &htlcView{}

	// We use two maps, one for the local log and one for the remote log to
	// keep track of which entries we need to skip when creating the final
	// htlc view. We skip an entry whenever we find a settle or a timeout
	// modifying an entry.
	skipUs := make(map[uint32]struct{})
	skipThem := make(map[uint32]struct{})

	// First we run through non-add entries in both logs, populating the
	// skip sets and mutating the current chain state (crediting balances, etc) to
	// reflect the settle/timeout entry encountered.
	for _, entry := range view.ourUpdates {
		if entry.EntryType == Add {
			continue
		}

		addEntry := lc.theirLogIndex[entry.ParentIndex].Value.(*PaymentDescriptor)

		skipThem[addEntry.Index] = struct{}{}
		processRemoveEntry(entry, ourBalance, theirBalance,
			nextHeight, remoteChain, true)
	}
	for _, entry := range view.theirUpdates {
		if entry.EntryType == Add {
			continue
		}

		addEntry := lc.ourLogIndex[entry.ParentIndex].Value.(*PaymentDescriptor)

		skipUs[addEntry.Index] = struct{}{}
		processRemoveEntry(entry, ourBalance, theirBalance,
			nextHeight, remoteChain, false)
	}

	// Next we take a second pass through all the log entries, skipping any
	// settled HTLC's, and debiting the chain state balance due to any
	// newly added HTLC's.
	for _, entry := range view.ourUpdates {
		isAdd := entry.EntryType == Add
		if _, ok := skipUs[entry.Index]; !isAdd || ok {
			continue
		}

		processAddEntry(entry, ourBalance, theirBalance, nextHeight,
			remoteChain, false)
		newView.ourUpdates = append(newView.ourUpdates, entry)
	}
	for _, entry := range view.theirUpdates {
		isAdd := entry.EntryType == Add
		if _, ok := skipThem[entry.Index]; !isAdd || ok {
			continue
		}

		processAddEntry(entry, ourBalance, theirBalance, nextHeight,
			remoteChain, true)
		newView.theirUpdates = append(newView.theirUpdates, entry)
	}

	return newView
}

// processAddEntry evaluates the effect of an add entry within the HTLC log.
// If the HTLC hasn't yet been committed in either chain, then the height it
// was commited is updated. Keeping track of this inclusion height allows us to
// later compact the log once the change is fully committed in both chains.
func processAddEntry(htlc *PaymentDescriptor, ourBalance, theirBalance *btcutil.Amount,
	nextHeight uint64, remoteChain bool, isIncoming bool) {

	// If we're evaluating this entry for the remote chain (to create/view
	// a new commitment), then we'll may be updating the height this entry
	// was added to the chain. Otherwise, we may be updating the entry's
	// height w.r.t the local chain.
	var addHeight *uint64
	if remoteChain {
		addHeight = &htlc.addCommitHeightRemote
	} else {
		addHeight = &htlc.addCommitHeightLocal
	}

	if *addHeight != 0 {
		return
	}

	if isIncoming {
		// If this is a new incoming (un-committed) HTLC, then we need
		// to update their balance accordingly by subtracting the
		// amount of the HTLC that are funds pending.
		*theirBalance -= htlc.Amount
	} else {
		// Similarly, we need to debit our balance if this is an out
		// going HTLC to reflect the pending balance.
		*ourBalance -= htlc.Amount
	}

	*addHeight = nextHeight
}

// processRemoveEntry processes a log entry which settles or timesout a
// previously added HTLC. If the removal entry has already been processed, it
// is skipped.
func processRemoveEntry(htlc *PaymentDescriptor, ourBalance,
	theirBalance *btcutil.Amount, nextHeight uint64,
	remoteChain bool, isIncoming bool) {

	var removeHeight *uint64
	if remoteChain {
		removeHeight = &htlc.removeCommitHeightRemote
	} else {
		removeHeight = &htlc.removeCommitHeightLocal
	}

	// Ignore any removal entries which have already been processed.
	if *removeHeight != 0 {
		return
	}

	switch {
	// If an incoming HTLC is being settled, then this means that we've
	// received the preimage either from another sub-system, or the
	// upstream peer in the route. Therefore, we increase our balance by
	// the HTLC amount.
	case isIncoming && htlc.EntryType == Settle:
		*ourBalance += htlc.Amount
	// Otherwise, this HTLC is being timed out, therefore the value of the
	// HTLC should return to the remote party.
	case isIncoming && htlc.EntryType == Timeout:
		*theirBalance += htlc.Amount
	// If an outgoing HTLC is being settled, then this means that the
	// downstream party resented the preimage or learned of it via a
	// downstream peer. In either case, we credit their settled value with
	// the value of the HTLC.
	case !isIncoming && htlc.EntryType == Settle:
		*theirBalance += htlc.Amount
	// Otherwise, one of our outgoing HTLC's has timed out, so the value of
	// the HTLC should be returned to our settled balance.
	case !isIncoming && htlc.EntryType == Timeout:
		*ourBalance += htlc.Amount
	}

	*removeHeight = nextHeight
}

// SignNextCommitment signs a new commitment which includes any previous
// unsettled HTLCs, any new HTLCs, and any modifications to prior HTLCs
// committed in previous commitment updates. Signing a new commitment
// decrements the available revocation window by 1. After a successful method
// call, the remote party's commitment chain is extended by a new commitment
// which includes all updates to the HTLC log prior to this method invocation.
func (lc *LightningChannel) SignNextCommitment() ([]byte, uint32, error) {
	// Ensure that we have enough unused revocation hashes given to us by the
	// remote party. If the set is empty, then we're unable to create a new
	// state unless they first revoke a prior commitment transaction.
	if len(lc.revocationWindow) == 0 ||
		len(lc.usedRevocations) == InitialRevocationWindow {
		return nil, 0, ErrNoWindow
	}

	// Grab the next revocation hash and key to use for this new commitment
	// transaction, if no errors occur then this revocation tuple will be
	// moved to the used set.
	nextRevocation := lc.revocationWindow[0]
	remoteRevocationKey := nextRevocation.NextRevocationKey
	remoteRevocationHash := nextRevocation.NextRevocationHash

	// Create a new commitment view which will calculate the evaluated
	// state of the remote node's new commitment including our latest added
	// HTLC's. The view includes the latest balances for both sides on the
	// remote node's chain, and also update the addition height of any new
	// HTLC log entries.
	newCommitView, err := lc.fetchCommitmentView(true, lc.ourLogCounter,
		lc.theirLogCounter, remoteRevocationKey, remoteRevocationHash)
	if err != nil {
		return nil, 0, err
	}

	walletLog.Tracef("ChannelPoint(%v): extending remote chain to height %v",
		lc.channelState.ChanID, newCommitView.height)
	walletLog.Tracef("ChannelPoint(%v): remote chain: our_balance=%v, "+
		"their_balance=%v, commit_tx: %v", lc.channelState.ChanID,
		newCommitView.ourBalance, newCommitView.theirBalance,
		newLogClosure(func() string {
			return spew.Sdump(newCommitView.txn)
		}))

	// Sign their version of the new commitment transaction.
	hashCache := txscript.NewTxSigHashes(newCommitView.txn)
	sig, err := txscript.RawTxInWitnessSignature(newCommitView.txn,
		hashCache, 0, int64(lc.channelState.Capacity),
		lc.channelState.FundingRedeemScript, txscript.SigHashAll,
		lc.channelState.OurMultiSigKey)
	if err != nil {
		return nil, 0, err
	}

	// Extend the remote commitment chain by one with the addition of our
	// latest commitment update.
	lc.remoteCommitChain.addCommitment(newCommitView)

	// Move the now used revocation hash from the unused set to the used set.
	// We only do this at the end, as we know at this point the procedure will
	// succeed without any errors.
	lc.usedRevocations = append(lc.usedRevocations, nextRevocation)
	lc.revocationWindow[0] = nil // Avoid a GC leak.
	lc.revocationWindow = lc.revocationWindow[1:]

	// Strip off the sighash flag on the signature in order to send it over
	// the wire.
	return sig[:len(sig)], lc.theirLogCounter, nil
}

// ReceiveNewCommitment processs a signature for a new commitment state sent by
// the remote party. This method will should be called in response to the
// remote party initiating a new change, or when the remote party sends a
// signature fully accepting a new state we've initiated. If we are able to
// succesfully validate the signature, then the generated commitment is added
// to our local commitment chain. Once we send a revocation for our prior
// state, then this newly added commitment becomes our current accepted channel
// state.
func (lc *LightningChannel) ReceiveNewCommitment(rawSig []byte,
	ourLogIndex uint32) error {

	theirCommitKey := lc.channelState.TheirCommitKey
	theirMultiSigKey := lc.channelState.TheirMultiSigKey

	// We're receiving a new commitment which attempts to extend our local
	// commitment chain height by one, so fetch the proper revocation to
	// derive the key+hash needed to construct the new commitment view and
	// state.
	nextHeight := lc.currentHeight + 1
	revocation, err := lc.channelState.LocalElkrem.AtIndex(nextHeight)
	if err != nil {
		return err
	}
	revocationKey := deriveRevocationPubkey(theirCommitKey, revocation[:])
	revocationHash := fastsha256.Sum256(revocation[:])

	// With the revocation information calculated, construct the new
	// commitment view which includes all the entries we know of in their
	// HTLC log, and up to ourLogIndex in our HTLC log.
	localCommitmentView, err := lc.fetchCommitmentView(false, ourLogIndex,
		lc.theirLogCounter, revocationKey, revocationHash)
	if err != nil {
		return err
	}

	walletLog.Tracef("ChannelPoint(%v): extending local chain to height %v",
		lc.channelState.ChanID, localCommitmentView.height)
	walletLog.Tracef("ChannelPoint(%v): local chain: our_balance=%v, "+
		"their_balance=%v, commit_tx: %v", lc.channelState.ChanID,
		localCommitmentView.ourBalance, localCommitmentView.theirBalance,
		newLogClosure(func() string {
			return spew.Sdump(localCommitmentView.txn)
		}))

	// Construct the sighash of the commitment transaction corresponding to
	// this newly proposed state update.
	localCommitTx := localCommitmentView.txn
	multiSigScript := lc.channelState.FundingRedeemScript
	hashCache := txscript.NewTxSigHashes(localCommitTx)
	sigHash, err := txscript.CalcWitnessSigHash(multiSigScript, hashCache,
		txscript.SigHashAll, localCommitTx, 0, int64(lc.channelState.Capacity))
	if err != nil {
		// TODO(roasbeef): fetchview has already mutated the htlc's...
		//  * need to either roll-back, or make pure
		return err
	}

	// Ensure that the newly constructed commitment state has a valid
	// signature.
	sig, err := btcec.ParseSignature(rawSig, btcec.S256())
	if err != nil {
		return err
	} else if !sig.Verify(sigHash, theirMultiSigKey) {
		return fmt.Errorf("invalid commitment signature")
	}

	// The signature checks out, so we can now add the new commitment to
	// our local commitment chain.
	localCommitmentView.sig = rawSig
	lc.localCommitChain.addCommitment(localCommitmentView)

	return nil
}

// PendingUpdates returns a boolean value reflecting if there are any pending
// updates which need to be committed. The state machine has pending updates if
// the local log index on the local and remote chain tip aren't identical. This
// indicates that either we have pending updates they need to commit, or vice
// versa.
func (lc *LightningChannel) PendingUpdates() bool {
	fullySynced := (lc.localCommitChain.tip().ourMessageIndex ==
		lc.remoteCommitChain.tip().ourMessageIndex)

	return !fullySynced
}

// RevokeCurrentCommitment revokes the next lowest unrevoked commitment
// transaction in the local commitment chain. As a result the edge of our
// revocation window is extended by one, and the tail of our local commitment
// chain is advanced by a single commitment. This now lowest unrevoked
// commitment becomes our currently accepted state within the channel.
func (lc *LightningChannel) RevokeCurrentCommitment() (*lnwire.CommitRevocation, error) {
	theirCommitKey := lc.channelState.TheirCommitKey

	// Now that we've accept a new state transition, we send the remote
	// party the revocation for our current commitment state.
	revocationMsg := &lnwire.CommitRevocation{}
	currentRevocation, err := lc.channelState.LocalElkrem.AtIndex(lc.currentHeight)
	if err != nil {
		return nil, err
	}
	copy(revocationMsg.Revocation[:], currentRevocation[:])

	// Along with this revocation, we'll also send an additional extension
	// to our revocation window to the remote party.
	lc.revocationWindowEdge++
	revocationEdge, err := lc.channelState.LocalElkrem.AtIndex(lc.revocationWindowEdge)
	if err != nil {
		return nil, err
	}
	revocationMsg.NextRevocationKey = deriveRevocationPubkey(theirCommitKey,
		revocationEdge[:])
	revocationMsg.NextRevocationHash = fastsha256.Sum256(revocationEdge[:])

	walletLog.Tracef("ChannelPoint(%v): revoking height=%v, now at height=%v, window_edge=%v",
		lc.channelState.ChanID, lc.localCommitChain.tail().height,
		lc.currentHeight+1, lc.revocationWindowEdge)

	// Advance our tail, as we've revoked our previous state.
	lc.localCommitChain.advanceTail()

	lc.currentHeight++

	// TODO(roasbeef): update sent/received.
	tail := lc.localCommitChain.tail()
	lc.channelState.OurCommitTx = tail.txn
	lc.channelState.OurBalance = tail.ourBalance
	lc.channelState.TheirBalance = tail.theirBalance
	lc.channelState.OurCommitSig = tail.sig
	lc.channelState.NumUpdates++

	walletLog.Tracef("ChannelPoint(%v): state transition accepted: "+
		"our_balance=%v, their_balance=%v", lc.channelState.ChanID,
		tail.ourBalance, tail.theirBalance)

	// TODO(roasbeef): use RecordChannelDelta once fin
	if err := lc.channelState.FullSync(); err != nil {
		return nil, err
	}

	revocationMsg.ChannelPoint = lc.channelState.ChanID
	return revocationMsg, nil
}

// ReceiveRevocation processes a revocation sent by the remote party for the
// lowest unrevoked commitment within their commitment chain. We receive a
// revocation either during the initial session negotiation wherein revocation
// windows are extended, or in response to a state update that we initiate. If
// successful, then the remote commitment chain is advanced by a single
// commitment, and a log compaction is attempted. In addition, a slice of
// HTLC's which can be forwarded upstream are returned.
func (lc *LightningChannel) ReceiveRevocation(revMsg *lnwire.CommitRevocation) ([]*PaymentDescriptor, error) {
	// The revocation has a nil (zero) pre-image, then this should simply be
	// added to the end of the revocation window for the remote node.
	if bytes.Equal(zeroHash[:], revMsg.Revocation[:]) {
		lc.revocationWindow = append(lc.revocationWindow, revMsg)
		return nil, nil
	}

	ourCommitKey := lc.channelState.OurCommitKey
	currentRevocationKey := lc.channelState.TheirCurrentRevocation
	pendingRevocation := wire.ShaHash(revMsg.Revocation)

	// Ensure the new pre-image fits in properly within the elkrem receiver
	// tree. If this fails, then all other checks are skipped.
	// TODO(rosbeef): abstract into func
	remoteElkrem := lc.channelState.RemoteElkrem
	if err := remoteElkrem.AddNext(&pendingRevocation); err != nil {
		return nil, err
	}

	// Verify that the revocation public key we can derive using this
	// pre-image and our private key is identical to the revocation key we
	// were given for their current (prior) commitment transaction.
	revocationPriv := deriveRevocationPrivKey(ourCommitKey, pendingRevocation[:])
	if !revocationPriv.PubKey().IsEqual(currentRevocationKey) {
		return nil, fmt.Errorf("revocation key mismatch")
	}

	// Additionally, we need to ensure we were given the proper pre-image
	// to the revocation hash used within any current HTLC's.
	if !bytes.Equal(lc.channelState.TheirCurrentRevocationHash[:], zeroHash[:]) {
		revokeHash := fastsha256.Sum256(pendingRevocation[:])
		// TODO(roasbeef): rename to drop the "Their"
		if !bytes.Equal(lc.channelState.TheirCurrentRevocationHash[:], revokeHash[:]) {
			return nil, fmt.Errorf("revocation hash mismatch")
		}
	}

	// Advance the head of the revocation queue now that this revocation has
	// been verified. Additionally, extend the end of our unused revocation
	// queue with the newly extended revocation window update.
	nextRevocation := lc.usedRevocations[0]
	lc.channelState.TheirCurrentRevocation = nextRevocation.NextRevocationKey
	lc.channelState.TheirCurrentRevocationHash = nextRevocation.NextRevocationHash
	lc.usedRevocations[0] = nil // Prevent GC leak.
	lc.usedRevocations = lc.usedRevocations[1:]
	lc.revocationWindow = append(lc.revocationWindow, revMsg)

	walletLog.Tracef("ChannelPoint(%v): remote party accepted state transition, "+
		"revoked height %v, now at %v", lc.channelState.ChanID,
		lc.remoteCommitChain.tail().height,
		lc.remoteCommitChain.tail().height+1)

	// At this point, the revocation has been accepted, and we've rotated
	// the current revocation key+hash for the remote party. Therefore we
	// sync now to ensure the elkrem receiver state is consistent with the
	// current commitment height.
	if err := lc.channelState.SyncRevocation(); err != nil {
		return nil, err
	}

	// Since they revoked the current lowest height in their commitment
	// chain, we can advance their chain by a single commitment.
	lc.remoteCommitChain.advanceTail()

	remoteChainTail := lc.remoteCommitChain.tail().height
	localChainTail := lc.localCommitChain.tail().height

	// Now that we've verified the revocation update the state of the HTLC
	// log as we may be able to prune portions of it now, and update their
	// balance.
	var htlcsToForward []*PaymentDescriptor
	for e := lc.theirUpdateLog.Front(); e != nil; e = e.Next() {
		htlc := e.Value.(*PaymentDescriptor)

		if htlc.isForwarded {
			continue
		}

		// TODO(roasbeef): re-visit after adding persistence to HTLC's
		//  * either record add height, or set to N - 1
		uncomitted := (htlc.addCommitHeightRemote == 0 ||
			htlc.addCommitHeightLocal == 0)
		if htlc.EntryType == Add && uncomitted {
			continue
		}

		if htlc.EntryType == Add &&
			remoteChainTail >= htlc.addCommitHeightRemote &&
			localChainTail >= htlc.addCommitHeightLocal {
			htlc.isForwarded = true
			htlcsToForward = append(htlcsToForward, htlc)
		} else if htlc.EntryType != Add &&
			remoteChainTail >= htlc.removeCommitHeightRemote &&
			localChainTail >= htlc.removeCommitHeightLocal {
			htlc.isForwarded = true
			htlcsToForward = append(htlcsToForward, htlc)
		}
	}

	lc.compactLogs(lc.ourUpdateLog, lc.theirUpdateLog,
		localChainTail, remoteChainTail)

	return htlcsToForward, nil
}

// compactLogs performs garbage collection within the log removing HTLC's which
// have been removed from the point-of-view of the tail of both chains. The
// entries which timeout/settle HTLC's are also removed.
func (lc *LightningChannel) compactLogs(ourLog, theirLog *list.List,
	localChainTail, remoteChainTail uint64) {

	compactLog := func(logA, logB *list.List, indexB, indexA map[uint32]*list.Element) {
		var nextA *list.Element
		for e := logA.Front(); e != nil; e = nextA {
			nextA = e.Next()

			htlc := e.Value.(*PaymentDescriptor)
			if htlc.EntryType == Add {
				continue
			}

			// If the HTLC hasn't yet been removed from either
			// chain, the skip it.
			if htlc.removeCommitHeightRemote == 0 ||
				htlc.removeCommitHeightLocal == 0 {
				continue
			}

			// Otherwise if the height of the tail of both chains
			// is at least the height in which the HTLC was
			// removed, then evict the settle/timeout entry along
			// with the original add entry.
			if remoteChainTail >= htlc.removeCommitHeightRemote &&
				localChainTail >= htlc.removeCommitHeightLocal {
				parentLink := indexB[htlc.ParentIndex]
				parentIndex := parentLink.Value.(*PaymentDescriptor).Index
				logB.Remove(parentLink)

				logA.Remove(e)

				delete(indexB, parentIndex)
				delete(indexA, htlc.Index)
			}

		}
	}

	compactLog(ourLog, theirLog, lc.theirLogIndex, lc.ourLogIndex)
	compactLog(theirLog, ourLog, lc.ourLogIndex, lc.theirLogIndex)
}

// ExtendRevocationWindow extends our revocation window by a single revocation,
// increasing the number of new commitment updates the remote party can
// initiate without our cooperation.
func (lc *LightningChannel) ExtendRevocationWindow() (*lnwire.CommitRevocation, error) {
	/// TODO(roasbeef): error if window edge differs from tail by more than
	// InitialRevocationWindow

	revMsg := &lnwire.CommitRevocation{}
	revMsg.ChannelPoint = lc.channelState.ChanID

	nextHeight := lc.revocationWindowEdge + 1
	revocation, err := lc.channelState.LocalElkrem.AtIndex(nextHeight)
	if err != nil {
		return nil, err
	}

	theirCommitKey := lc.channelState.TheirCommitKey
	revMsg.NextRevocationKey = deriveRevocationPubkey(theirCommitKey,
		revocation[:])
	revMsg.NextRevocationHash = fastsha256.Sum256(revocation[:])

	lc.revocationWindowEdge++

	return revMsg, nil
}

// AddHTLC adds an HTLC to the state machine's local update log. This method
// should be called when preparing to send an outgoing HTLC.
func (lc *LightningChannel) AddHTLC(htlc *lnwire.HTLCAddRequest) uint32 {
	pd := &PaymentDescriptor{
		EntryType: Add,
		RHash:     PaymentHash(htlc.RedemptionHashes[0]),
		Timeout:   htlc.Expiry,
		Amount:    btcutil.Amount(htlc.Amount),
		Index:     lc.ourLogCounter,
	}

	lc.ourLogIndex[pd.Index] = lc.ourUpdateLog.PushBack(pd)
	lc.ourLogCounter++

	return pd.Index
}

// ReceiveHTLC adds an HTLC to the state machine's remote update log. This
// method should be called in response to receiving a new HTLC from the remote
// party.
func (lc *LightningChannel) ReceiveHTLC(htlc *lnwire.HTLCAddRequest) uint32 {
	pd := &PaymentDescriptor{
		EntryType: Add,
		RHash:     PaymentHash(htlc.RedemptionHashes[0]),
		Timeout:   htlc.Expiry,
		Amount:    btcutil.Amount(htlc.Amount),
		Index:     lc.theirLogCounter,
	}

	lc.theirLogIndex[pd.Index] = lc.theirUpdateLog.PushBack(pd)
	lc.theirLogCounter++

	return pd.Index
}

// SettleHTLC attempst to settle an existing outstanding received HTLC. The
// remote log index of the HTLC settled is returned in order to facilitate
// creating the corresponding wire message. In the case the supplied pre-image
// is invalid, an error is returned.
func (lc *LightningChannel) SettleHTLC(preimage [32]byte) (uint32, error) {
	var targetHTLC *list.Element

	// TODO(roasbeef): optimize
	paymentHash := fastsha256.Sum256(preimage[:])
	for e := lc.theirUpdateLog.Front(); e != nil; e = e.Next() {
		htlc := e.Value.(*PaymentDescriptor)
		if htlc.EntryType != Add {
			continue
		}

		if !htlc.settled && bytes.Equal(htlc.RHash[:], paymentHash[:]) {
			htlc.settled = true
			targetHTLC = e
			break
		}
	}
	if targetHTLC == nil {
		return 0, fmt.Errorf("invalid payment hash")
	}

	parentPd := targetHTLC.Value.(*PaymentDescriptor)

	// TODO(roasbeef): maybe make the log entries an interface?
	pd := &PaymentDescriptor{
		Amount:      parentPd.Amount,
		Index:       lc.ourLogCounter,
		ParentIndex: parentPd.Index,
		EntryType:   Settle,
	}

	lc.ourUpdateLog.PushBack(pd)
	lc.ourLogCounter++

	return targetHTLC.Value.(*PaymentDescriptor).Index, nil
}

// ReceiveHTLCSettle attempts to settle an existing outgoing HTLC indexed by an
// index into the local log. If the specified index doesn't exist within the
// log, and error is returned. Similarly if the preimage is invalid w.r.t to
// the referenced of then a distinct error is returned.
func (lc *LightningChannel) ReceiveHTLCSettle(preimage [32]byte, logIndex uint32) error {
	paymentHash := fastsha256.Sum256(preimage[:])
	addEntry, ok := lc.ourLogIndex[logIndex]
	if !ok {
		return fmt.Errorf("non existant log entry")
	}

	htlc := addEntry.Value.(*PaymentDescriptor)
	if !bytes.Equal(htlc.RHash[:], paymentHash[:]) {
		return fmt.Errorf("invalid payment hash")
	}

	pd := &PaymentDescriptor{
		Amount:      htlc.Amount,
		ParentIndex: htlc.Index,
		Index:       lc.theirLogCounter,
		EntryType:   Settle,
	}

	lc.theirUpdateLog.PushBack(pd)
	lc.theirLogCounter++

	return nil
}

// TimeoutHTLC...
func (lc *LightningChannel) TimeoutHTLC() error {
	return nil
}

// ChannelPoint returns the outpoint of the original funding transaction which
// created this active channel. This outpoint is used throughout various
// sub-systems to uniquely identify an open channel.
func (lc *LightningChannel) ChannelPoint() *wire.OutPoint {
	return lc.channelState.ChanID
}

// addHTLC adds a new HTLC to the passed commitment transaction. One of four
// full scripts will be generated for the HTLC output depending on if the HTLC
// is incoming and if it's being applied to our commitment transaction or that
// of the remote node's.
func (lc *LightningChannel) addHTLC(commitTx *wire.MsgTx, ourCommit bool,
	paymentDesc *PaymentDescriptor, revocation [32]byte, delay uint32,
	isIncoming bool) error {

	localKey := lc.channelState.OurCommitKey.PubKey()
	remoteKey := lc.channelState.TheirCommitKey
	timeout := paymentDesc.Timeout
	rHash := paymentDesc.RHash

	// Generate the proper redeem scripts for the HTLC output modified by
	// two-bits denoting if this is an incoming HTLC, and if the HTLC is
	// being applied to their commitment transaction or ours.
	var pkScript []byte
	var err error
	switch {
	// The HTLC is paying to us, and being applied to our commitment
	// transaction. So we need to use the receiver's version of HTLC the
	// script.
	case isIncoming && ourCommit:
		pkScript, err = receiverHTLCScript(timeout, delay, remoteKey,
			localKey, revocation[:], rHash[:])
	// We're being paid via an HTLC by the remote party, and the HTLC is
	// being added to their commitment transaction, so we use the sender's
	// version of the HTLC script.
	case isIncoming && !ourCommit:
		pkScript, err = senderHTLCScript(timeout, delay, remoteKey,
			localKey, revocation[:], rHash[:])
	// We're sending an HTLC which is being added to our commitment
	// transaction. Therefore, we need to use the sender's version of the
	// HTLC script.
	case !isIncoming && ourCommit:
		pkScript, err = senderHTLCScript(timeout, delay, localKey,
			remoteKey, revocation[:], rHash[:])
	// Finally, we're paying the remote party via an HTLC, which is being
	// added to their commitment transaction. Therefore, we use the
	// receiver's version of the HTLC script.
	case !isIncoming && !ourCommit:
		pkScript, err = receiverHTLCScript(timeout, delay, localKey,
			remoteKey, revocation[:], rHash[:])
	}
	if err != nil {
		return err
	}

	// Now that we have the redeem scripts, create the P2WSH public key
	// script for the output itself.
	htlcP2WSH, err := witnessScriptHash(pkScript)
	if err != nil {
		return err
	}

	// Add the new HTLC outputs to the respective commitment transactions.
	amountPending := int64(paymentDesc.Amount)
	commitTx.AddTxOut(wire.NewTxOut(amountPending, htlcP2WSH))

	return nil
}

// ForceClose...
func (lc *LightningChannel) ForceClose() error {
	return nil
}

// InitCooperativeClose initiates a cooperative closure of an active lightning
// channel. This method should only be executed once all pending HTLCs (if any)
// on the channel have been cleared/removed. Upon completion, the source channel
// will shift into the "closing" state, which indicates that all incoming/outgoing
// HTLC requests should be rejected. A signature for the closing transaction,
// and the txid of the closing transaction are returned. The initiator of the
// channel closure should then watch the blockchain for a confirmation of the
// closing transaction before considering the channel terminated. In the case
// of an unresponsive remote party, the initiator can either choose to execute
// a force closure, or backoff for a period of time, and retry the cooperative
// closure.
// TODO(roasbeef): caller should initiate signal to reject all incoming HTLCs,
// settle any inflight.
func (lc *LightningChannel) InitCooperativeClose() ([]byte, *wire.ShaHash, error) {
	lc.Lock()
	defer lc.Unlock()

	// If we're already closing the channel, then ignore this request.
	if lc.status == channelClosing || lc.status == channelClosed {
		// TODO(roasbeef): check to ensure no pending payments
		return nil, nil, ErrChanClosing
	}

	// Otherwise, indicate in the channel status that a channel closure has
	// been initiated.
	lc.status = channelClosing

	// TODO(roasbeef): assumes initiator pays fees
	closeTx := createCooperativeCloseTx(lc.fundingTxIn,
		lc.channelState.OurBalance, lc.channelState.TheirBalance,
		lc.channelState.OurDeliveryScript, lc.channelState.TheirDeliveryScript,
		true)
	closeTxSha := closeTx.TxSha()

	// Finally, sign the completed cooperative closure transaction. As the
	// initiator we'll simply send our signature over the the remote party,
	// using the generated txid to be notified once the closure transaction
	// has been confirmed.
	hashCache := txscript.NewTxSigHashes(closeTx)
	closeSig, err := txscript.RawTxInWitnessSignature(closeTx,
		hashCache, 0, int64(lc.channelState.Capacity),
		lc.channelState.FundingRedeemScript, txscript.SigHashAll,
		lc.channelState.OurMultiSigKey)
	if err != nil {
		return nil, nil, err
	}

	return closeSig, &closeTxSha, nil
}

// CompleteCooperativeClose completes the cooperative closure of the target
// active lightning channel. This method should be called in response to the
// remote node initating a cooperative channel closure. A fully signed closure
// transaction is returned. It is the duty of the responding node to broadcast
// a signed+valid closure transaction to the network.
func (lc *LightningChannel) CompleteCooperativeClose(remoteSig []byte) (*wire.MsgTx, error) {
	lc.Lock()
	defer lc.Unlock()

	// If we're already closing the channel, then ignore this request.
	if lc.status == channelClosing || lc.status == channelClosed {
		// TODO(roasbeef): check to ensure no pending payments
		return nil, ErrChanClosing
	}

	lc.status = channelClosed

	// Create the transaction used to return the current settled balance
	// on this active channel back to both parties. In this current model,
	// the initiator pays full fees for the cooperative close transaction.
	closeTx := createCooperativeCloseTx(lc.fundingTxIn,
		lc.channelState.OurBalance, lc.channelState.TheirBalance,
		lc.channelState.OurDeliveryScript, lc.channelState.TheirDeliveryScript,
		false)

	// With the transaction created, we can finally generate our half of
	// the 2-of-2 multi-sig needed to redeem the funding output.
	redeemScript := lc.channelState.FundingRedeemScript
	hashCache := txscript.NewTxSigHashes(closeTx)
	capacity := int64(lc.channelState.Capacity)
	closeSig, err := txscript.RawTxInWitnessSignature(closeTx,
		hashCache, 0, capacity, redeemScript, txscript.SigHashAll,
		lc.channelState.OurMultiSigKey)
	if err != nil {
		return nil, err
	}

	// Finally, construct the witness stack minding the order of the
	// pubkeys+sigs on the stack.
	ourKey := lc.channelState.OurMultiSigKey.PubKey().SerializeCompressed()
	theirKey := lc.channelState.TheirMultiSigKey.SerializeCompressed()
	witness := spendMultiSig(redeemScript, ourKey, closeSig,
		theirKey, remoteSig)
	closeTx.TxIn[0].Witness = witness

	// Validate the finalized transaction to ensure the output script is
	// properly met, and that the remote peer supplied a valid signature.
	vm, err := txscript.NewEngine(lc.fundingP2WSH, closeTx, 0,
		txscript.StandardVerifyFlags, nil, hashCache, capacity)
	if err != nil {
		return nil, err
	}
	if err := vm.Execute(); err != nil {
		return nil, err
	}

	return closeTx, nil
}

// DeleteState deletes all state concerning the channel from the underlying
// database, only leaving a small summary describing meta-data of the
// channel's lifetime.
func (lc *LightningChannel) DeleteState() error {
	return lc.channelState.CloseChannel()
}

// StateSnapshot returns a snapshot b
func (lc *LightningChannel) StateSnapshot() *channeldb.ChannelSnapshot {
	lc.stateMtx.RLock()
	defer lc.stateMtx.RUnlock()

	return lc.channelState.Snapshot()
}

// createCommitTx creates a commitment transaction, spending from specified
// funding output. The commitment transaction contains two outputs: one paying
// to the "owner" of the commitment transaction which can be spent after a
// relative block delay or revocation event, and the other paying the the
// counter-party within the channel, which can be spent immediately.
func createCommitTx(fundingOutput *wire.TxIn, selfKey, theirKey *btcec.PublicKey,
	revokeKey *btcec.PublicKey, csvTimeout uint32, amountToSelf,
	amountToThem btcutil.Amount) (*wire.MsgTx, error) {

	// First, we create the script for the delayed "pay-to-self" output.
	// This output has 2 main redemption clauses: either we can redeem the
	// output after a relative block delay, or the remote node can claim
	// the funds with the revocation key if we broadcast a revoked
	// commitment transaction.
	ourRedeemScript, err := commitScriptToSelf(csvTimeout, selfKey,
		revokeKey)
	if err != nil {
		return nil, err
	}
	payToUsScriptHash, err := witnessScriptHash(ourRedeemScript)
	if err != nil {
		return nil, err
	}

	// Next, we create the script paying to them. This is just a regular
	// P2WKH output, without any added CSV delay.
	theirWitnessKeyHash, err := commitScriptUnencumbered(theirKey)
	if err != nil {
		return nil, err
	}

	// Now that both output scripts have been created, we can finally create
	// the transaction itself. We use a transaction version of 2 since CSV
	// will fail unless the tx version is >= 2.
	commitTx := wire.NewMsgTx()
	commitTx.Version = 2
	commitTx.AddTxIn(fundingOutput)

	// Avoid creating zero value outputs within the commitment transaction.
	if amountToSelf != 0 {
		commitTx.AddTxOut(wire.NewTxOut(int64(amountToSelf), payToUsScriptHash))
	}
	if amountToThem != 0 {
		commitTx.AddTxOut(wire.NewTxOut(int64(amountToThem), theirWitnessKeyHash))
	}

	return commitTx, nil
}

// createCooperativeCloseTx creates a transaction which if signed by both
// parties, then broadcast cooperatively closes an active channel. The creation
// of the closure transaction is modified by a boolean indicating if the party
// constructing the channel is the initiator of the closure. Currently it is
// expected that the initiator pays the transaction fees for the closing
// transaction in full.
func createCooperativeCloseTx(fundingTxIn *wire.TxIn,
	ourBalance, theirBalance btcutil.Amount,
	ourDeliveryScript, theirDeliveryScript []byte,
	initiator bool) *wire.MsgTx {

	// Construct the transaction to perform a cooperative closure of the
	// channel. In the event that one side doesn't have any settled funds
	// within the channel then a refund output for that particular side can
	// be omitted.
	closeTx := wire.NewMsgTx()
	closeTx.AddTxIn(fundingTxIn)

	// The initiator the a cooperative closure pays the fee in entirety.
	// Determine if we're the initiator so we can compute fees properly.
	// @XXX nadav: no fees for now
	/*if initiator {
		// TODO(roasbeef): take sat/byte here instead of properly calc
		ourBalance -= 5000
	} else {
		theirBalance -= 5000
	}*/

	// TODO(roasbeef): dust check...
	//  * although upper layers should prevent
	if ourBalance != 0 {
		closeTx.AddTxOut(&wire.TxOut{
			PkScript: ourDeliveryScript,
			Value:    int64(ourBalance),
		})
	}
	if theirBalance != 0 {
		closeTx.AddTxOut(&wire.TxOut{
			PkScript: theirDeliveryScript,
			Value:    int64(theirBalance),
		})
	}

	txsort.InPlaceSort(closeTx)

	closeTx, err := ColorifyTx(closeTx, false)
	if err != nil {
		// nadav @TODO return (error, MsgTx) and propagate errors
		log.Fatal("unable to colorify: %v", err)
	}

	return closeTx
}
