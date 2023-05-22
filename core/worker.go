package core

import (
	"errors"
	"fmt"
	"math/big"
	"sync/atomic"
	"time"

	mapset "github.com/deckarep/golang-set"
	"github.com/dominant-strategies/go-quai/common"
	"github.com/dominant-strategies/go-quai/common/hexutil"
	"github.com/dominant-strategies/go-quai/consensus"
	"github.com/dominant-strategies/go-quai/consensus/misc"
	"github.com/dominant-strategies/go-quai/core/rawdb"
	"github.com/dominant-strategies/go-quai/core/state"
	"github.com/dominant-strategies/go-quai/core/types"
	"github.com/dominant-strategies/go-quai/ethdb"
	"github.com/dominant-strategies/go-quai/event"
	"github.com/dominant-strategies/go-quai/log"
	"github.com/dominant-strategies/go-quai/params"
	"github.com/dominant-strategies/go-quai/trie"
	lru "github.com/hashicorp/golang-lru"
	sync "github.com/sasha-s/go-deadlock"
)

const (
	// resultQueueSize is the size of channel listening to sealing result.
	resultQueueSize = 10

	// txChanSize is the size of channel listening to NewTxsEvent.
	// The number is referenced from the size of tx pool.
	txChanSize = 4096

	// resubmitAdjustChanSize is the size of resubmitting interval adjustment channel.
	resubmitAdjustChanSize = 10

	// sealingLogAtDepth is the number of confirmations before logging successful sealing.
	sealingLogAtDepth = 7

	// minRecommitInterval is the minimal time interval to recreate the sealing block with
	// any newly arrived transactions.
	minRecommitInterval = 1 * time.Second

	// staleThreshold is the maximum depth of the acceptable stale block.
	staleThreshold = 7

	// pendingBlockBodyLimit is maximum number of pending block bodies to be kept in cache.
	pendingBlockBodyLimit = 1024
)

// environment is the worker's current environment and holds all
// information of the sealing block generation.
type environment struct {
	signer types.Signer

	state     *state.StateDB // apply state changes here
	ancestors mapset.Set     // ancestor set (used for checking uncle parent validity)
	family    mapset.Set     // family set (used for checking uncle invalidity)
	tcount    int            // tx count in cycle
	gasPool   *GasPool       // available gas used to pack transactions
	coinbase  common.Address

	header              *types.Header
	txs                 []*types.Transaction
	etxs                []*types.Transaction
	subManifest         types.BlockManifest
	receipts            []*types.Receipt
	uncleMu             sync.RWMutex
	uncles              map[common.Hash]*types.Header
	externalGasUsed     uint64
	externalBlockLength int
}

// copy creates a deep copy of environment.
func (env *environment) copy() *environment {
	nodeCtx := common.NodeLocation.Context()
	if nodeCtx == common.ZONE_CTX {
		cpy := &environment{
			signer:    env.signer,
			state:     env.state.Copy(),
			ancestors: env.ancestors.Clone(),
			family:    env.family.Clone(),
			tcount:    env.tcount,
			coinbase:  env.coinbase,
			header:    types.CopyHeader(env.header),
			receipts:  copyReceipts(env.receipts),
		}
		if env.gasPool != nil {
			gasPool := *env.gasPool
			cpy.gasPool = &gasPool
		}
		// The content of txs and uncles are immutable, unnecessary
		// to do the expensive deep copy for them.
		cpy.txs = make([]*types.Transaction, len(env.txs))
		copy(cpy.txs, env.txs)
		cpy.etxs = make([]*types.Transaction, len(env.etxs))
		copy(cpy.etxs, env.etxs)

		env.uncleMu.Lock()
		cpy.uncles = make(map[common.Hash]*types.Header)
		for hash, uncle := range env.uncles {
			cpy.uncles[hash] = uncle
		}
		env.uncleMu.Unlock()
		return cpy
	} else {
		return &environment{header: types.CopyHeader(env.header)}
	}
}

// unclelist returns the contained uncles as the list format.
func (env *environment) unclelist() []*types.Header {
	env.uncleMu.RLock()
	defer env.uncleMu.RUnlock()
	var uncles []*types.Header
	for _, uncle := range env.uncles {
		uncles = append(uncles, uncle)
	}
	return uncles
}

// discard terminates the background prefetcher go-routine. It should
// always be called for all created environment instances otherwise
// the go-routine leak can happen.
func (env *environment) discard() {
	if env.state == nil {
		return
	}
	env.state.StopPrefetcher()
}

// task contains all information for consensus engine sealing and result submitting.
type task struct {
	receipts  []*types.Receipt
	state     *state.StateDB
	block     *types.Block
	createdAt time.Time
}

const (
	commitInterruptNone int32 = iota
	commitInterruptNewHead
	commitInterruptResubmit
)

// intervalAdjust represents a resubmitting interval adjustment.
type intervalAdjust struct {
	ratio float64
	inc   bool
}

// Config is the configuration parameters of mining.
type Config struct {
	Etherbase  common.Address `toml:",omitempty"` // Public address for block mining rewards (default = first account)
	Notify     []string       `toml:",omitempty"` // HTTP URL list to be notified of new work packages (only useful in ethash).
	NotifyFull bool           `toml:",omitempty"` // Notify with pending block headers instead of work packages
	ExtraData  hexutil.Bytes  `toml:",omitempty"` // Block extra data set by the miner
	GasFloor   uint64         // Target gas floor for mined blocks.
	GasCeil    uint64         // Target gas ceiling for mined blocks.
	GasPrice   *big.Int       // Minimum gas price for mining a transaction
	Recommit   time.Duration  // The time interval for miner to re-create mining work.
	Noverify   bool           // Disable remote mining solution verification(only useful in ethash).
}

// worker is the main object which takes care of submitting new work to consensus engine
// and gathering the sealing result.
type worker struct {
	config      *Config
	chainConfig *params.ChainConfig
	engine      consensus.Engine
	hc          *HeaderChain
	txPool      *TxPool

	// Feeds
	pendingLogsFeed   event.Feed
	pendingHeaderFeed event.Feed

	// Subscriptions
	txsCh       chan NewTxsEvent
	txsSub      event.Subscription
	chainHeadCh chan ChainHeadEvent

	// Channels
	taskCh             chan *task
	resultCh           chan *types.Block
	exitCh             chan struct{}
	resubmitIntervalCh chan time.Duration
	resubmitAdjustCh   chan *intervalAdjust

	wg sync.WaitGroup

	current      *environment                 // An environment for current running cycle.
	localUncles  map[common.Hash]*types.Block // A set of side blocks generated locally as the possible uncle blocks.
	remoteUncles map[common.Hash]*types.Block // A set of side blocks as the possible uncle blocks.
	uncleMu      sync.RWMutex

	mu       sync.RWMutex // The lock used to protect the coinbase and extra fields
	coinbase common.Address
	extra    []byte

	workerDb ethdb.Database

	pendingBlockBody *lru.Cache

	snapshotMu       sync.RWMutex // The lock used to protect the snapshots below
	snapshotBlock    *types.Block
	snapshotReceipts types.Receipts
	snapshotState    *state.StateDB

	// atomic status counters
	running int32 // The indicator whether the consensus engine is running or not.
	newTxs  int32 // New arrival transaction count since last sealing work submitting.

	// noempty is the flag used to control whether the feature of pre-seal empty
	// block is enabled. The default value is false(pre-seal is enabled by default).
	// But in some special scenario the consensus engine will seal blocks instantaneously,
	// in this case this feature will add all empty blocks into canonical chain
	// non-stop and no real transaction will be included.
	noempty uint32

	// External functions
	isLocalBlock func(header *types.Header) bool // Function used to determine whether the specified block is mined by local miner.

	// Test hooks
	newTaskHook  func(*task) // Method to call upon receiving a new sealing task.
	fullTaskHook func()      // Method to call before pushing the full sealing task.
}

func newWorker(config *Config, chainConfig *params.ChainConfig, db ethdb.Database, engine consensus.Engine, headerchain *HeaderChain, txPool *TxPool, isLocalBlock func(header *types.Header) bool, init bool) *worker {
	worker := &worker{
		config:             config,
		chainConfig:        chainConfig,
		engine:             engine,
		hc:                 headerchain,
		txPool:             txPool,
		coinbase:           config.Etherbase,
		isLocalBlock:       isLocalBlock,
		workerDb:           db,
		localUncles:        make(map[common.Hash]*types.Block),
		remoteUncles:       make(map[common.Hash]*types.Block),
		txsCh:              make(chan NewTxsEvent, txChanSize),
		chainHeadCh:        make(chan ChainHeadEvent, chainHeadChanSize),
		taskCh:             make(chan *task),
		resultCh:           make(chan *types.Block, resultQueueSize),
		exitCh:             make(chan struct{}),
		resubmitIntervalCh: make(chan time.Duration),
		resubmitAdjustCh:   make(chan *intervalAdjust, resubmitAdjustChanSize),
	}
	nodeCtx := common.NodeLocation.Context()

	phBodyCache, _ := lru.New(pendingBlockBodyLimit)
	worker.pendingBlockBody = phBodyCache

	if nodeCtx == common.ZONE_CTX {
		// Subscribe NewTxsEvent for tx pool
		worker.txsSub = txPool.SubscribeNewTxsEvent(worker.txsCh)
	}

	// Sanitize recommit interval if the user-specified one is too short.
	recommit := worker.config.Recommit
	if recommit < minRecommitInterval {
		log.Warn("Sanitizing miner recommit interval", "provided", recommit, "updated", minRecommitInterval)
		recommit = minRecommitInterval
	}

	worker.wg.Add(1)
	go worker.mainLoop()

	return worker
}

// setEtherbase sets the etherbase used to initialize the block coinbase field.
func (w *worker) setEtherbase(addr common.Address) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.coinbase = addr
}

func (w *worker) setGasCeil(ceil uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.config.GasCeil = ceil
}

// setExtra sets the content used to initialize the block extra field.
func (w *worker) setExtra(extra []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.extra = extra
}

// setRecommitInterval updates the interval for miner sealing work recommitting.
func (w *worker) setRecommitInterval(interval time.Duration) {
	select {
	case w.resubmitIntervalCh <- interval:
	case <-w.exitCh:
	}
}

// disablePreseal disables pre-sealing feature
func (w *worker) disablePreseal() {
	atomic.StoreUint32(&w.noempty, 1)
}

// enablePreseal enables pre-sealing feature
func (w *worker) enablePreseal() {
	atomic.StoreUint32(&w.noempty, 0)
}

// pending returns the pending state and corresponding block.
func (w *worker) pending() (*types.Block, *state.StateDB) {
	// return a snapshot to avoid contention on currentMu mutex
	w.snapshotMu.RLock()
	defer w.snapshotMu.RUnlock()
	if w.snapshotState == nil {
		return nil, nil
	}
	return w.snapshotBlock, w.snapshotState.Copy()
}

// pendingBlock returns pending block.
func (w *worker) pendingBlock() *types.Block {
	// return a snapshot to avoid contention on currentMu mutex
	w.snapshotMu.RLock()
	defer w.snapshotMu.RUnlock()
	return w.snapshotBlock
}

// pendingBlockAndReceipts returns pending block and corresponding receipts.
func (w *worker) pendingBlockAndReceipts() (*types.Block, types.Receipts) {
	// return a snapshot to avoid contention on currentMu mutex
	w.snapshotMu.RLock()
	defer w.snapshotMu.RUnlock()
	return w.snapshotBlock, w.snapshotReceipts
}

// start sets the running status as 1 and triggers new work submitting.
func (w *worker) start() {
	atomic.StoreInt32(&w.running, 1)
}

// stop sets the running status as 0.
func (w *worker) stop() {
	atomic.StoreInt32(&w.running, 0)
}

// isRunning returns an indicator whether worker is running or not.
func (w *worker) isRunning() bool {
	return atomic.LoadInt32(&w.running) == 1
}

// close terminates all background threads maintained by the worker.
// Note the worker does not support being closed multiple times.
func (w *worker) close() {
	atomic.StoreInt32(&w.running, 0)
	close(w.exitCh)
	w.wg.Wait()
}

func (w *worker) LoadPendingBlockBody() {
	pendingBlockBodykeys := rawdb.ReadPbBodyKeys(w.workerDb)
	for _, key := range pendingBlockBodykeys {
		if key == types.EmptyBodyHash {
			w.pendingBlockBody.Add(key, &types.Body{})
		} else {
			w.pendingBlockBody.Add(key, rawdb.ReadPbCacheBody(w.workerDb, key))
		}
		// Remove the entry from the database so that body is not accumulated over multiple stops
		rawdb.DeletePbCacheBody(w.workerDb, key)
	}
}

// StorePendingBlockBody stores the pending block body cache into the db
func (w *worker) StorePendingBlockBody() {
	// store the pendingBodyCache body
	var pendingBlockBodyKeys []common.Hash
	pendingBlockBody := w.pendingBlockBody
	for _, key := range pendingBlockBody.Keys() {
		if value, exist := pendingBlockBody.Peek(key); exist {
			pendingBlockBodyKeys = append(pendingBlockBodyKeys, key.(common.Hash))
			if key.(common.Hash) != types.EmptyBodyHash {
				rawdb.WritePbCacheBody(w.workerDb, key.(common.Hash), value.(*types.Body))
			}
		}
	}
	rawdb.WritePbBodyKeys(w.workerDb, pendingBlockBodyKeys)
}

// GeneratePendingBlock generates pending block given a commited block.
func (w *worker) GeneratePendingHeader(block *types.Block, fill bool) (*types.Header, error) {
	nodeCtx := common.NodeLocation.Context()

	// Sanitize recommit interval if the user-specified one is too short.
	recommit := w.config.Recommit
	if recommit < minRecommitInterval {
		log.Warn("Sanitizing miner recommit interval", "provided", recommit, "updated", minRecommitInterval)
		recommit = minRecommitInterval
	}

	var (
		interrupt *int32
		timestamp int64 // timestamp for each round of sealing.
	)

	timer := time.NewTimer(0)
	defer timer.Stop()
	<-timer.C // discard the initial tick

	timestamp = time.Now().Unix()
	if interrupt != nil {
		atomic.StoreInt32(interrupt, commitInterruptNewHead)
	}
	interrupt = new(int32)

	// reset the timer and update the newTx to zero.
	timer.Reset(recommit)
	atomic.StoreInt32(&w.newTxs, 0)

	start := time.Now()
	// Set the coinbase if the worker is running or it's required
	var coinbase common.Address
	if w.coinbase.Equal(common.ZeroAddr) {
		log.Error("Refusing to mine without etherbase")
		return nil, errors.New("etherbase not found")
	}
	coinbase = w.coinbase // Use the preset address as the fee recipient

	work, err := w.prepareWork(&generateParams{
		timestamp: uint64(timestamp),
		coinbase:  coinbase,
	}, block)
	if err != nil {
		return nil, err
	}

	if nodeCtx == common.ZONE_CTX {
		// Fill pending transactions from the txpool
		w.adjustGasLimit(nil, work, block)
		if fill {
			w.fillTransactions(interrupt, work, block)
		}
	}

	env := work.copy()

	// Swap out the old work with the new one, terminating any leftover
	// prefetcher processes in the mean time and starting a new one.
	if w.current != nil {
		w.current.discard()
	}
	w.current = work

	// Create a local environment copy, avoid the data race with snapshot state.
	// https://github.com/ethereum/go-ethereum/issues/24299
	block, err = w.FinalizeAssembleAndBroadcast(w.hc, env.header, block, env.state, env.txs, env.unclelist(), env.etxs, env.subManifest, env.receipts)
	if err != nil {
		return nil, err
	}
	env.header = block.Header()

	env.uncleMu.RLock()
	if w.CurrentInfo(block.Header()) {
		log.Info("Commit new sealing work", "number", block.Number(), "sealhash", block.Header().SealHash(),
			"uncles", len(env.uncles), "txs", env.tcount, "etxs", len(block.ExtTransactions()),
			"gas", block.GasUsed(), "fees", totalFees(block, env.receipts),
			"elapsed", common.PrettyDuration(time.Since(start)))
	} else {
		log.Debug("Commit new sealing work", "number", block.Number(), "sealhash", block.Header().SealHash(),
			"uncles", len(env.uncles), "txs", env.tcount, "etxs", len(block.ExtTransactions()),
			"gas", block.GasUsed(), "fees", totalFees(block, env.receipts),
			"elapsed", common.PrettyDuration(time.Since(start)))
	}
	env.uncleMu.RUnlock()

	w.updateSnapshot(env)

	return w.snapshotBlock.Header(), nil
}

// mainLoop is responsible for generating and submitting sealing work based on
// the received event. It can support two modes: automatically generate task and
// submit it or return task according to given parameters for various proposes.
func (w *worker) mainLoop() {
	nodeCtx := common.NodeLocation.Context()
	defer w.wg.Done()
	if nodeCtx == common.ZONE_CTX {
		defer w.txsSub.Unsubscribe()
	}
	defer func() {
		if w.current != nil {
			w.current.discard()
		}
	}()

	cleanTicker := time.NewTicker(time.Second * 10)
	defer cleanTicker.Stop()

	if nodeCtx == common.ZONE_CTX {
		go w.eventExitLoop()
	}

	for {
		select {
		case <-cleanTicker.C:
			chainHead := w.hc.CurrentBlock()
			w.uncleMu.RLock()
			for hash, uncle := range w.localUncles {
				if uncle.NumberU64()+staleThreshold <= chainHead.NumberU64() {
					delete(w.localUncles, hash)
				}
			}
			for hash, uncle := range w.remoteUncles {
				if uncle.NumberU64()+staleThreshold <= chainHead.NumberU64() {
					delete(w.remoteUncles, hash)
				}
			}
			w.uncleMu.RUnlock()

		case ev := <-w.txsCh:

			// Apply transactions to the pending state if we're not sealing
			//
			// Note all transactions received may not be continuous with transactions
			// already included in the current sealing block. These transactions will
			// be automatically eliminated.
			if !w.isRunning() && w.current != nil {
				// If block is already full, abort
				if gp := w.current.gasPool; gp != nil && gp.Gas() < params.TxGas {
					continue
				}
				txs := make(map[common.AddressBytes]types.Transactions)
				for _, tx := range ev.Txs {
					acc, _ := types.Sender(w.current.signer, tx)
					txs[acc.Bytes20()] = append(txs[acc.Bytes20()], tx)
				}
				txset := types.NewTransactionsByPriceAndNonce(w.current.signer, txs, w.current.header.BaseFee(), false)
				tcount := w.current.tcount
				w.commitTransactions(w.current, txset, nil)

				// Only update the snapshot if any new transactions were added
				// to the pending block
				if tcount != w.current.tcount {
					w.updateSnapshot(w.current)
				}
			}
			atomic.AddInt32(&w.newTxs, int32(len(ev.Txs)))

		}
	}
}

func (w *worker) eventExitLoop() {
	for {
		select {
		case <-w.exitCh:
			return
		case <-w.txsSub.Err():
			return
		}
	}
}

// makeEnv creates a new environment for the sealing block.
func (w *worker) makeEnv(parent *types.Block, header *types.Header, coinbase common.Address) (*environment, error) {
	// Retrieve the parent state to execute on top and start a prefetcher for
	// the miner to speed block sealing up a bit.
	state, err := w.hc.bc.processor.StateAt(parent.Root())
	if err != nil {
		// Note since the sealing block can be created upon the arbitrary parent
		// block, but the state of parent block may already be pruned, so the necessary
		// state recovery is needed here in the future.
		//
		// The maximum acceptable reorg depth can be limited by the finalised block
		// somehow. TODO(rjl493456442) fix the hard-coded number here later.
		state, err = w.hc.bc.processor.StateAtBlock(parent, 1024, nil, false)
		log.Warn("Recovered mining state", "root", parent.Root(), "err", err)
	}
	if err != nil {
		return nil, err
	}
	state.StartPrefetcher("miner")

	// Note the passed coinbase may be different with header.Coinbase.
	env := &environment{
		signer:          types.MakeSigner(w.chainConfig, header.Number()),
		state:           state,
		coinbase:        coinbase,
		ancestors:       mapset.NewSet(),
		family:          mapset.NewSet(),
		header:          header,
		uncles:          make(map[common.Hash]*types.Header),
		externalGasUsed: uint64(0),
	}
	// when 08 is processed ancestors contain 07 (quick block)
	for _, ancestor := range w.hc.GetBlocksFromHash(parent.Hash(), 7) {
		for _, uncle := range ancestor.Uncles() {
			env.family.Add(uncle.Hash())
		}
		env.family.Add(ancestor.Hash())
		env.ancestors.Add(ancestor.Hash())
	}
	// Keep track of transactions which return errors so they can be removed
	env.tcount = 0
	return env, nil
}

// commitUncle adds the given block to uncle block set, returns error if failed to add.
func (w *worker) commitUncle(env *environment, uncle *types.Header) error {
	env.uncleMu.Lock()
	defer env.uncleMu.Unlock()
	hash := uncle.Hash()
	if _, exist := env.uncles[hash]; exist {
		return errors.New("uncle not unique")
	}
	if env.header.ParentHash() == uncle.ParentHash() {
		return errors.New("uncle is sibling")
	}
	if !env.ancestors.Contains(uncle.ParentHash()) {
		return errors.New("uncle's parent unknown")
	}
	if env.family.Contains(hash) {
		return errors.New("uncle already included")
	}
	env.uncles[hash] = uncle
	return nil
}

// updateSnapshot updates pending snapshot block, receipts and state.
func (w *worker) updateSnapshot(env *environment) {
	w.snapshotMu.Lock()
	defer w.snapshotMu.Unlock()
	nodeCtx := common.NodeLocation.Context()

	w.snapshotBlock = types.NewBlock(
		env.header,
		env.txs,
		env.unclelist(),
		env.etxs,
		env.subManifest,
		env.receipts,
		trie.NewStackTrie(nil),
	)
	if nodeCtx == common.ZONE_CTX {
		w.snapshotReceipts = copyReceipts(env.receipts)
		w.snapshotState = env.state.Copy()
	}
}

func (w *worker) commitTransaction(env *environment, tx *types.Transaction) ([]*types.Log, error) {
	if tx != nil {
		snap := env.state.Snapshot()
		// retrieve the gas used int and pass in the reference to the ApplyTransaction
		gasUsed := env.header.GasUsed()
		receipt, err := ApplyTransaction(w.chainConfig, w.hc, &env.coinbase, env.gasPool, env.state, env.header, tx, &gasUsed, *w.hc.bc.processor.GetVMConfig())
		if err != nil {
			log.Debug("Error playing transaction in worker", "err", err, "tx", tx.Hash().Hex(), "block", env.header.Number, "gasUsed", gasUsed)
			env.state.RevertToSnapshot(snap)
			return nil, err
		}
		// once the gasUsed pointer is updated in the ApplyTransaction it has to be set back to the env.Header.GasUsed
		// This extra step is needed because previously the GasUsed was a public method and direct update of the value
		// was possible.
		env.header.SetGasUsed(gasUsed)

		env.txs = append(env.txs, tx)
		env.receipts = append(env.receipts, receipt)
		if receipt.Status == types.ReceiptStatusSuccessful {
			for _, etx := range receipt.Etxs {
				env.etxs = append(env.etxs, etx)
			}
		}
		return receipt.Logs, nil
	}
	return nil, errors.New("error finding transaction")
}

func (w *worker) commitTransactions(env *environment, txs *types.TransactionsByPriceAndNonce, interrupt *int32) bool {
	gasLimit := env.header.GasLimit
	if env.gasPool == nil {
		env.gasPool = new(GasPool).AddGas(gasLimit())
	}
	var coalescedLogs []*types.Log

	for {
		// In the following three cases, we will interrupt the execution of the transaction.
		// (1) new head block event arrival, the interrupt signal is 1
		// (2) worker start or restart, the interrupt signal is 1
		// (3) worker recreate the sealing block with any newly arrived transactions, the interrupt signal is 2.
		// For the first two cases, the semi-finished work will be discarded.
		// For the third case, the semi-finished work will be submitted to the consensus engine.
		if interrupt != nil && atomic.LoadInt32(interrupt) != commitInterruptNone {
			// Notify resubmit loop to increase resubmitting interval due to too frequent commits.
			if atomic.LoadInt32(interrupt) == commitInterruptResubmit {
				ratio := float64(gasLimit()-env.gasPool.Gas()) / float64(gasLimit())
				if ratio < 0.1 {
					ratio = 0.1
				}
				w.resubmitAdjustCh <- &intervalAdjust{
					ratio: ratio,
					inc:   true,
				}
			}
			return atomic.LoadInt32(interrupt) == commitInterruptNewHead
		}
		// If we don't have enough gas for any further transactions then we're done
		if env.gasPool.Gas() < params.TxGas {
			log.Trace("Not enough gas for further transactions", "have", env.gasPool, "want", params.TxGas)
			break
		}
		// Retrieve the next transaction and abort if all done
		tx := txs.Peek()
		if tx == nil {
			break
		}
		// Error may be ignored here. The error has already been checked
		// during transaction acceptance is the transaction pool.
		//
		// We use the signer regardless of the current hf.
		from, _ := types.Sender(env.signer, tx)
		// Start executing the transaction
		env.state.Prepare(tx.Hash(), env.tcount)

		logs, err := w.commitTransaction(env, tx)
		switch {
		case errors.Is(err, ErrGasLimitReached):
			// Pop the current out-of-gas transaction without shifting in the next from the account
			log.Trace("Gas limit exceeded for current block", "sender", from)
			txs.Pop()

		case errors.Is(err, ErrNonceTooLow):
			// New head notification data race between the transaction pool and miner, shift
			log.Trace("Skipping transaction with low nonce", "sender", from, "nonce", tx.Nonce())
			txs.Shift(from.Bytes20(), false)

		case errors.Is(err, ErrNonceTooHigh):
			// Reorg notification data race between the transaction pool and miner, skip account =
			log.Debug("Skipping account with high nonce", "sender", from, "nonce", tx.Nonce())
			txs.Pop()

		case errors.Is(err, nil):
			// Everything ok, collect the logs and shift in the next transaction from the same account
			coalescedLogs = append(coalescedLogs, logs...)
			env.tcount++
			txs.Shift(from.Bytes20(), false)

		case errors.Is(err, ErrTxTypeNotSupported):
			// Pop the unsupported transaction without shifting in the next from the account
			log.Error("Skipping unsupported transaction type", "sender", from, "type", tx.Type())
			txs.Pop()

		default:
			// Strange error, discard the transaction and get the next in line (note, the
			// nonce-too-high clause will prevent us from executing in vain).
			log.Debug("Transaction failed, account skipped", "hash", tx.Hash(), "err", err)
			txs.Shift(from.Bytes20(), false)
		}
	}

	if !w.isRunning() && len(coalescedLogs) > 0 {
		// We don't push the pendingLogsEvent while we are sealing. The reason is that
		// when we are sealing, the worker will regenerate a sealing block every 3 seconds.
		// In order to avoid pushing the repeated pendingLog, we disable the pending log pushing.

		// make a copy, the state caches the logs and these logs get "upgraded" from pending to mined
		// logs by filling in the block hash when the block was mined by the local miner. This can
		// cause a race condition if a log was "upgraded" before the PendingLogsEvent is processed.
		cpy := make([]*types.Log, len(coalescedLogs))
		for i, l := range coalescedLogs {
			cpy[i] = new(types.Log)
			*cpy[i] = *l
		}
		w.pendingLogsFeed.Send(cpy)
	}
	return false
}

// generateParams wraps various of settings for generating sealing task.
type generateParams struct {
	timestamp uint64         // The timstamp for sealing task
	forceTime bool           // Flag whether the given timestamp is immutable or not
	coinbase  common.Address // The fee recipient address for including transaction
}

// prepareWork constructs the sealing task according to the given parameters,
// either based on the last chain head or specified parent. In this function
// the pending transactions are not filled yet, only the empty task returned.
func (w *worker) prepareWork(genParams *generateParams, block *types.Block) (*environment, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	nodeCtx := common.NodeLocation.Context()

	// Find the parent block for sealing task
	parent := block
	// Sanity check the timestamp correctness, recap the timestamp
	// to parent+1 if the mutation is allowed.
	timestamp := genParams.timestamp
	if parent.Time() >= timestamp {
		if genParams.forceTime {
			return nil, fmt.Errorf("invalid timestamp, parent %d given %d", parent.Time(), timestamp)
		}
		timestamp = parent.Time() + 1
	}
	// Construct the sealing block header, set the extra field if it's allowed
	num := parent.Number()
	header := types.EmptyHeader()
	header.SetParentHash(block.Header().Hash())
	header.SetNumber(big.NewInt(int64(num.Uint64()) + 1))
	header.SetTime(timestamp)

	// Only calculate entropy if the parent is not the genesis block
	if parent.Hash() != w.hc.config.GenesisHash {
		order, err := parent.Header().CalcOrder()
		if err != nil {
			return nil, err
		}
		// Set the parent delta S prior to sending to sub
		if nodeCtx != common.PRIME_CTX {
			if order < nodeCtx {
				header.SetParentDeltaS(big.NewInt(0), nodeCtx)
			} else {
				header.SetParentDeltaS(parent.Header().CalcDeltaS(), nodeCtx)
			}
		}
		header.SetParentEntropy(parent.Header().CalcS())
	}

	// Only zone should calculate state
	if nodeCtx == common.ZONE_CTX {
		header.SetExtra(w.extra)
		header.SetBaseFee(misc.CalcBaseFee(w.chainConfig, parent.Header()))
		if w.isRunning() {
			if w.coinbase.Equal(common.ZeroAddr) {
				log.Error("Refusing to mine without etherbase")
				return nil, errors.New("refusing to mine without etherbase")
			}
			header.SetCoinbase(w.coinbase)
		}

		// Run the consensus preparation with the default or customized consensus engine.
		if err := w.engine.Prepare(w.hc, header, block.Header()); err != nil {
			log.Error("Failed to prepare header for sealing", "err", err)
			return nil, err
		}
		env, err := w.makeEnv(parent, header, w.coinbase)
		if err != nil {
			log.Error("Failed to create sealing context", "err", err)
			return nil, err
		}
		// Accumulate the uncles for the sealing work.
		commitUncles := func(blocks map[common.Hash]*types.Block) {
			for hash, uncle := range blocks {
				env.uncleMu.RLock()
				if len(env.uncles) == 2 {
					env.uncleMu.RUnlock()
					break
				}
				env.uncleMu.RUnlock()
				if err := w.commitUncle(env, uncle.Header()); err != nil {
					log.Trace("Possible uncle rejected", "hash", hash, "reason", err)
				} else {
					log.Debug("Committing new uncle to block", "hash", hash)
				}
			}
		}
		w.uncleMu.RLock()
		// Prefer to locally generated uncle
		commitUncles(w.localUncles)
		commitUncles(w.remoteUncles)
		w.uncleMu.RUnlock()
		return env, nil
	} else {
		return &environment{header: header}, nil
	}

}

// fillTransactions retrieves the pending transactions from the txpool and fills them
// into the given sealing block. The transaction selection and ordering strategy can
// be customized with the plugin in the future.
func (w *worker) fillTransactions(interrupt *int32, env *environment, block *types.Block) {
	// Split the pending transactions into locals and remotes
	// Fill the block with all available pending transactions.
	etxSet := rawdb.ReadEtxSet(w.hc.bc.db, block.Hash(), block.NumberU64())
	if etxSet == nil {
		return
	}
	pending, err := w.txPool.TxPoolPending(true, etxSet)
	if err != nil {
		return
	}
	localTxs, remoteTxs := make(map[common.AddressBytes]types.Transactions), pending
	for _, account := range w.txPool.Locals() {
		if txs := remoteTxs[account.Bytes20()]; len(txs) > 0 {
			delete(remoteTxs, account.Bytes20())
			localTxs[account.Bytes20()] = txs
		}
	}
	if len(localTxs) > 0 {
		txs := types.NewTransactionsByPriceAndNonce(env.signer, localTxs, env.header.BaseFee(), false)
		if w.commitTransactions(env, txs, interrupt) {
			return
		}
	}
	if len(remoteTxs) > 0 {
		txs := types.NewTransactionsByPriceAndNonce(env.signer, remoteTxs, env.header.BaseFee(), false)
		if w.commitTransactions(env, txs, interrupt) {
			return
		}
	}
}

// fillTransactions retrieves the pending transactions from the txpool and fills them
// into the given sealing block. The transaction selection and ordering strategy can
// be customized with the plugin in the future.
func (w *worker) adjustGasLimit(interrupt *int32, env *environment, parent *types.Block) {

	gasUsed := (parent.GasUsed() + env.externalGasUsed) / uint64(env.externalBlockLength+1)

	env.header.SetGasLimit(CalcGasLimit(parent.GasLimit(), gasUsed))
}

func (w *worker) FinalizeAssembleAndBroadcast(chain consensus.ChainHeaderReader, header *types.Header, parent *types.Block, state *state.StateDB, txs []*types.Transaction, uncles []*types.Header, etxs []*types.Transaction, subManifest types.BlockManifest, receipts []*types.Receipt) (*types.Block, error) {
	nodeCtx := common.NodeLocation.Context()
	block, err := w.engine.FinalizeAndAssemble(chain, header, state, txs, uncles, etxs, subManifest, receipts)
	if err != nil {
		return nil, err
	}

	// Compute and set manifest hash
	manifest := types.BlockManifest{}
	if nodeCtx == common.PRIME_CTX {
		// Nothing to do for prime chain
		manifest = types.BlockManifest{}
	} else if w.engine.IsDomCoincident(parent.Header()) {
		manifest = types.BlockManifest{parent.Hash()}
	} else {
		manifest, err = w.hc.CollectBlockManifest(parent.Header())
		if err != nil {
			return nil, err
		}
		manifest = append(manifest, header.ParentHash())
	}
	manifestHash := types.DeriveSha(manifest, trie.NewStackTrie(nil))
	block.Header().SetManifestHash(manifestHash)

	if nodeCtx == common.ZONE_CTX {
		// Compute and set etx rollup hash
		etxRollup := types.Transactions{}
		if w.engine.IsDomCoincident(parent.Header()) {
			etxRollup = parent.ExtTransactions()
		} else {
			etxRollup, err = w.hc.CollectEtxRollup(parent)
			if err != nil {
				return nil, err
			}
			etxRollup = append(etxRollup, parent.ExtTransactions()...)
		}
		etxRollupHash := types.DeriveSha(etxRollup, trie.NewStackTrie(nil))
		block.Header().SetEtxRollupHash(etxRollupHash)
	}

	w.AddPendingBlockBody(block.Header(), block.Body())

	return block, nil
}

// commit runs any post-transaction state modifications, assembles the final block
// and commits new work if consensus engine is running.
// Note the assumption is held that the mutation is allowed to the passed env, do
// the deep copy first.
func (w *worker) commit(env *environment, interval func(), update bool, start time.Time) error {
	if w.isRunning() {
		if interval != nil {
			interval()
		}
		// Create a local environment copy, avoid the data race with snapshot state.
		// https://github.com/ethereum/go-ethereum/issues/24299
		env := env.copy()
		parent := w.hc.GetBlock(env.header.ParentHash(), env.header.NumberU64()-1)
		block, err := w.FinalizeAssembleAndBroadcast(w.hc, env.header, parent, env.state, env.txs, env.unclelist(), env.etxs, env.subManifest, env.receipts)
		if err != nil {
			return err
		}
		env.header = block.Header()
		select {
		case w.taskCh <- &task{receipts: env.receipts, state: env.state, block: block, createdAt: time.Now()}:
			env.uncleMu.RLock()
			log.Info("Commit new sealing work", "number", block.Number(), "sealhash", block.Header().SealHash(),
				"uncles", len(env.uncles), "txs", env.tcount, "etxs", len(block.ExtTransactions()),
				"gas", block.GasUsed(), "fees", totalFees(block, env.receipts),
				"elapsed", common.PrettyDuration(time.Since(start)))
			env.uncleMu.RUnlock()
		case <-w.exitCh:
			log.Info("worker has exited")
		}

	}
	if update {
		w.updateSnapshot(env)
	}
	return nil
}

// GetPendingBlockBodyKey takes a header and hashes all the Roots together
// and returns the key to be used for the pendingBlockBodyCache.
func (w *worker) getPendingBlockBodyKey(header *types.Header) common.Hash {
	return types.RlpHash([]interface{}{
		header.UncleHash(),
		header.TxHash(),
		header.EtxHash(),
	})
}

// AddPendingBlockBody adds an entry in the lru cache for the given pendingBodyKey
// maps it to body.
func (w *worker) AddPendingBlockBody(header *types.Header, body *types.Body) {
	w.pendingBlockBody.ContainsOrAdd(w.getPendingBlockBodyKey(header), body)
}

// GetPendingBlockBody gets the block body associated with the given header.
func (w *worker) GetPendingBlockBody(header *types.Header) *types.Body {
	key := w.getPendingBlockBodyKey(header)
	body, ok := w.pendingBlockBody.Get(key)
	if ok {
		return body.(*types.Body)
	}
	log.Warn("pending block body not found for header: ", key)
	return nil
}

// copyReceipts makes a deep copy of the given receipts.
func copyReceipts(receipts []*types.Receipt) []*types.Receipt {
	result := make([]*types.Receipt, len(receipts))
	for i, l := range receipts {
		cpy := *l
		result[i] = &cpy
	}
	return result
}

// totalFees computes total consumed miner fees in ETH. Block transactions and receipts have to have the same order.
func totalFees(block *types.Block, receipts []*types.Receipt) *big.Float {
	feesWei := new(big.Int)
	for i, tx := range block.Transactions() {
		minerFee, _ := tx.EffectiveGasTip(block.BaseFee())
		feesWei.Add(feesWei, new(big.Int).Mul(new(big.Int).SetUint64(receipts[i].GasUsed), minerFee))
	}
	return new(big.Float).Quo(new(big.Float).SetInt(feesWei), new(big.Float).SetInt(big.NewInt(params.Ether)))
}

func (w *worker) CurrentInfo(header *types.Header) bool {
	return header.NumberU64()+c_startingPrintLimit > w.hc.CurrentHeader().NumberU64()
}
