// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package miner

import (
	"errors"
	"fmt"
	"math/big"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/misc"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/trie"
)

const (
	// resultQueueSize is the size of channel listening to sealing result.
	resultQueueSize = 10

	// txChanSize is the size of channel listening to NewTxsEvent.
	// The number is referenced from the size of tx pool.
	txChanSize = 4096

	// chainHeadChanSize is the size of channel listening to ChainHeadEvent.
	chainHeadChanSize = 10

	// resubmitAdjustChanSize is the size of resubmitting interval adjustment channel.
	resubmitAdjustChanSize = 10

	// minRecommitInterval is the minimal time interval to recreate the sealing block with
	// any newly arrived transactions.
	minRecommitInterval = 1 * time.Second

	// maxRecommitInterval is the maximum time interval to recreate the sealing block with
	// any newly arrived transactions.
	maxRecommitInterval = 15 * time.Second

	// intervalAdjustRatio is the impact a single interval adjustment has on sealing work
	// resubmitting interval.
	intervalAdjustRatio = 0.1

	// intervalAdjustBias is applied during the new resubmit interval calculation in favor of
	// increasing upper limit or decreasing lower limit so that the limit can be reachable.
	intervalAdjustBias = 200 * 1000.0 * 1000.0

	// staleThreshold is the maximum depth of the acceptable stale block.
	staleThreshold = 7
)

var (
	errBlockInterruptedByNewHead  = errors.New("new head arrived while building block")
	errBlockInterruptedByRecommit = errors.New("recommit interrupt while building block")
	errBlockInterruptedByTimeout  = errors.New("timeout while building block")
)

// environment is the worker's current environment and holds all
// information of the sealing block generation.
type environment struct {
	signer   types.Signer
	state    *state.StateDB // apply state changes here
	tcount   int            // tx count in cycle
	gasPool  *core.GasPool  // available gas used to pack transactions
	coinbase common.Address

	header   *types.Header
	txs      []*types.Transaction
	receipts []*types.Receipt
}

// copy creates a deep copy of environment.
func (env *environment) copy() *environment {
	cpy := &environment{
		signer:   env.signer,
		state:    env.state.Copy(),
		tcount:   env.tcount,
		coinbase: env.coinbase,
		header:   types.CopyHeader(env.header),
		receipts: copyReceipts(env.receipts),
	}
	if env.gasPool != nil {
		gasPool := *env.gasPool
		cpy.gasPool = &gasPool
	}
	cpy.txs = make([]*types.Transaction, len(env.txs))
	copy(cpy.txs, env.txs)
	return cpy
}

// discard terminates the background prefetcher go-routine. It should
// always be called for all created environment instances otherwise
// the go-routine leak can happen.
func (env *environment) discard() {
	if env.state == nil {
		return
	}
	env.state.StopPrefetcher() // TODO: user here
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
	commitInterruptTimeout
)

// newWorkReq represents a request for new sealing work submitting with relative interrupt notifier.
type newWorkReq struct {
	interrupt *atomic.Int32
	timestamp int64
}

// newPayloadResult represents a result struct corresponds to payload generation.
type newPayloadResult struct {
	err   error
	block *types.Block
	fees  *big.Int
}

// getWorkReq represents a request for getting a new sealing work with provided parameters.
type getWorkReq struct {
	params *generateParams
	result chan *newPayloadResult // non-blocking channel
}

// intervalAdjust represents a resubmitting interval adjustment.
type intervalAdjust struct {
	ratio float64
	inc   bool
}

// worker is the main object which takes care of submitting new work to consensus engine
// and gathering the sealing result.
type worker struct {
	config      *Config
	chainConfig *params.ChainConfig
	engine      consensus.Engine
	eth         Backend
	chain       *core.BlockChain

	// Feeds
	pendingLogsFeed event.Feed

	// Subscriptions
	mux          *event.TypeMux
	txsCh        chan core.NewTxsEvent
	txsSub       event.Subscription
	chainHeadCh  chan core.ChainHeadEvent
	chainHeadSub event.Subscription

	// Channels
	newWorkCh          chan *newWorkReq
	getWorkCh          chan *getWorkReq
	taskCh             chan *task
	resultCh           chan *types.Block
	startCh            chan struct{}
	exitCh             chan struct{}
	resubmitIntervalCh chan time.Duration
	resubmitAdjustCh   chan *intervalAdjust

	wg sync.WaitGroup

	current *environment // An environment for current running cycle.

	mu       sync.RWMutex // The lock used to protect the coinbase and extra fields
	coinbase common.Address
	extra    []byte

	pendingMu    sync.RWMutex
	pendingTasks map[common.Hash]*task

	snapshotMu       sync.RWMutex // The lock used to protect the snapshots below
	snapshotBlock    *types.Block
	snapshotReceipts types.Receipts
	snapshotState    *state.StateDB

	// atomic status counters
	running atomic.Bool  // The indicator whether the consensus engine is running or not.
	newTxs  atomic.Int32 // New arrival transaction count since last sealing work submitting.
	syncing atomic.Bool  // The indicator whether the node is still syncing.

	// newpayloadTimeout is the maximum timeout allowance for creating payload.
	// The default value is 2 seconds but node operator can set it to arbitrary
	// large value. A large timeout allowance may cause Geth to fail creating
	// a non-empty payload within the specified time and eventually miss the slot
	// in case there are some computation expensive transactions in txpool.
	newpayloadTimeout time.Duration

	// recommit is the time interval to re-create sealing work or to re-build
	// payload in proof-of-stake stage.
	recommit time.Duration

	// External functions
	isLocalBlock func(header *types.Header) bool // Function used to determine whether the specified block is mined by local miner.

	// Test hooks
	newTaskHook  func(*task)                        // Method to call upon receiving a new sealing task.
	skipSealHook func(*task) bool                   // Method to decide whether skipping the sealing.
	fullTaskHook func()                             // Method to call before pushing the full sealing task.
	resubmitHook func(time.Duration, time.Duration) // Method to call upon updating resubmitting interval.
}

func newWorker(config *Config, chainConfig *params.ChainConfig, engine consensus.Engine, eth Backend, mux *event.TypeMux, isLocalBlock func(header *types.Header) bool, init bool) *worker {
	worker := &worker{
		config:             config,
		chainConfig:        chainConfig,
		engine:             engine,
		eth:                eth,
		chain:              eth.BlockChain(),
		mux:                mux,
		isLocalBlock:       isLocalBlock,
		coinbase:           config.Etherbase,
		extra:              config.ExtraData,
		pendingTasks:       make(map[common.Hash]*task),
		txsCh:              make(chan core.NewTxsEvent, txChanSize),
		chainHeadCh:        make(chan core.ChainHeadEvent, chainHeadChanSize),
		newWorkCh:          make(chan *newWorkReq),
		getWorkCh:          make(chan *getWorkReq),
		taskCh:             make(chan *task),
		resultCh:           make(chan *types.Block, resultQueueSize),
		startCh:            make(chan struct{}, 1),
		exitCh:             make(chan struct{}),
		resubmitIntervalCh: make(chan time.Duration),
		resubmitAdjustCh:   make(chan *intervalAdjust, resubmitAdjustChanSize),
	}
	// Subscribe NewTxsEvent for tx pool
	worker.txsSub = eth.TxPool().SubscribeNewTxsEvent(worker.txsCh)
	// Subscribe events for blockchain
	worker.chainHeadSub = eth.BlockChain().SubscribeChainHeadEvent(worker.chainHeadCh)

	// Sanitize recommit interval if the user-specified one is too short.
	recommit := worker.config.Recommit
	if recommit < minRecommitInterval {
		log.Warn("Sanitizing miner recommit interval", "provided", recommit, "updated", minRecommitInterval)
		recommit = minRecommitInterval
	}
	worker.recommit = recommit

	// Sanitize the timeout config for creating payload.
	newpayloadTimeout := worker.config.NewPayloadTimeout
	if newpayloadTimeout == 0 {
		log.Warn("Sanitizing new payload timeout to default", "provided", newpayloadTimeout, "updated", DefaultConfig.NewPayloadTimeout)
		newpayloadTimeout = DefaultConfig.NewPayloadTimeout
	}
	if newpayloadTimeout < time.Millisecond*100 {
		log.Warn("Low payload timeout may cause high amount of non-full blocks", "provided", newpayloadTimeout, "default", DefaultConfig.NewPayloadTimeout)
	}
	worker.newpayloadTimeout = newpayloadTimeout

	worker.wg.Add(4)
	go worker.mainLoop()
	go worker.newWorkLoop(recommit)
	go worker.resultLoop()
	go worker.taskLoop()

	// Submit first work to initialize pending state.
	if init {
		worker.startCh <- struct{}{}
	}
	return worker
}

// setEtherbase sets the etherbase used to initialize the block coinbase field.
func (w *worker) setEtherbase(addr common.Address) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.coinbase = addr
}

// etherbase retrieves the configured etherbase address.
func (w *worker) etherbase() common.Address {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.coinbase
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

// pending returns the pending state and corresponding block. The returned
// values can be nil in case the pending block is not initialized.
func (w *worker) pending() (*types.Block, *state.StateDB) {
	w.snapshotMu.RLock()
	defer w.snapshotMu.RUnlock()
	if w.snapshotState == nil {
		return nil, nil
	}
	return w.snapshotBlock, w.snapshotState.Copy()
}

// pendingBlock returns pending block. The returned block can be nil in case the
// pending block is not initialized.
func (w *worker) pendingBlock() *types.Block {
	w.snapshotMu.RLock()
	defer w.snapshotMu.RUnlock()
	return w.snapshotBlock
}

// pendingBlockAndReceipts returns pending block and corresponding receipts.
// The returned values can be nil in case the pending block is not initialized.
func (w *worker) pendingBlockAndReceipts() (*types.Block, types.Receipts) {
	w.snapshotMu.RLock()
	defer w.snapshotMu.RUnlock()
	return w.snapshotBlock, w.snapshotReceipts
}

// start sets the running status as 1 and triggers new work submitting.
func (w *worker) start() {
	w.running.Store(true)
	w.startCh <- struct{}{}
}

// stop sets the running status as 0.
func (w *worker) stop() {
	w.running.Store(false)
}

// isRunning returns an indicator whether worker is running or not.
func (w *worker) isRunning() bool {
	return w.running.Load()
}

// close terminates all background threads maintained by the worker.
// Note the worker does not support being closed multiple times.
func (w *worker) close() {
	w.running.Store(false)
	close(w.exitCh)
	w.wg.Wait()
}

// recalcRecommit recalculates the resubmitting interval upon feedback.
func recalcRecommit(minRecommit, prev time.Duration, target float64, inc bool) time.Duration {
	var (
		prevF = float64(prev.Nanoseconds())
		next  float64
	)
	if inc {
		next = prevF*(1-intervalAdjustRatio) + intervalAdjustRatio*(target+intervalAdjustBias)
		max := float64(maxRecommitInterval.Nanoseconds())
		if next > max {
			next = max
		}
	} else {
		next = prevF*(1-intervalAdjustRatio) + intervalAdjustRatio*(target-intervalAdjustBias)
		min := float64(minRecommit.Nanoseconds())
		if next < min {
			next = min
		}
	}
	return time.Duration(int64(next))
}

// newWorkLoop is a standalone goroutine to submit new sealing work upon received events.
func (w *worker) newWorkLoop(recommit time.Duration) {
	defer w.wg.Done()
	var (
		interrupt   *atomic.Int32
		minRecommit = recommit // minimal resubmit interval specified by user.
		timestamp   int64      // timestamp for each round of sealing.
	)

	timer := time.NewTimer(0)
	defer timer.Stop()
	<-timer.C // discard the initial tick

	// commit aborts in-flight transaction execution with given signal and resubmits a new one.
	commit := func(s int32) {
		if interrupt != nil {
			interrupt.Store(s)
		}
		interrupt = new(atomic.Int32)
		select {
		case w.newWorkCh <- &newWorkReq{interrupt: interrupt, timestamp: timestamp}:
		case <-w.exitCh:
			return
		}
		timer.Reset(recommit)
		w.newTxs.Store(0)
	}
	// clearPending cleans the stale pending tasks.
	clearPending := func(number uint64) {
		w.pendingMu.Lock()
		for h, t := range w.pendingTasks {
			if t.block.NumberU64()+staleThreshold <= number {
				delete(w.pendingTasks, h)
			}
		}
		w.pendingMu.Unlock()
	}

	for {
		select {
		case <-w.startCh:
			clearPending(w.chain.CurrentBlock().Number.Uint64())
			timestamp = time.Now().Unix()
			commit(commitInterruptNewHead)

		case head := <-w.chainHeadCh:
			clearPending(head.Block.NumberU64())
			timestamp = time.Now().Unix()
			commit(commitInterruptNewHead)

		case <-timer.C:
			// If sealing is running resubmit a new work cycle periodically to pull in
			// higher priced transactions. Disable this overhead for pending blocks.
			if w.isRunning() && (w.chainConfig.Clique == nil || w.chainConfig.Clique.Period > 0) {
				// Short circuit if no new transaction arrives.
				if w.newTxs.Load() == 0 {
					timer.Reset(recommit)
					continue
				}
				commit(commitInterruptResubmit)
			}

		case interval := <-w.resubmitIntervalCh:
			// Adjust resubmit interval explicitly by user.
			if interval < minRecommitInterval {
				log.Warn("Sanitizing miner recommit interval", "provided", interval, "updated", minRecommitInterval)
				interval = minRecommitInterval
			}
			log.Info("Miner recommit interval update", "from", minRecommit, "to", interval)
			minRecommit, recommit = interval, interval

			if w.resubmitHook != nil {
				w.resubmitHook(minRecommit, recommit)
			}

		case adjust := <-w.resubmitAdjustCh:
			// Adjust resubmit interval by feedback.
			if adjust.inc {
				before := recommit
				target := float64(recommit.Nanoseconds()) / adjust.ratio
				recommit = recalcRecommit(minRecommit, recommit, target, true)
				log.Trace("Increase miner recommit interval", "from", before, "to", recommit)
			} else {
				before := recommit
				recommit = recalcRecommit(minRecommit, recommit, float64(minRecommit.Nanoseconds()), false)
				log.Trace("Decrease miner recommit interval", "from", before, "to", recommit)
			}

			if w.resubmitHook != nil {
				w.resubmitHook(minRecommit, recommit)
			}

		case <-w.exitCh:
			return
		}
	}
}

// mainLoop 负责根据接收到的事件生成并提交密封工作.
// It can support two modes: automatically generate task and
// submit it or return task according to given parameters for various proposes.
func (w *worker) mainLoop() {
	defer w.wg.Done()
	defer w.txsSub.Unsubscribe()
	defer w.chainHeadSub.Unsubscribe()
	defer func() {
		if w.current != nil {
			w.current.discard()
		}
	}()

	for {
		select {
		case req := <-w.newWorkCh:
			w.commitWork(req.interrupt, req.timestamp) // !!! 走的这一条路

		case req := <-w.getWorkCh:
			block, fees, err := w.generateWork(req.params)
			req.result <- &newPayloadResult{
				err:   err,
				block: block,
				fees:  fees,
			}

		case ev := <-w.txsCh:
			// TODO: 交易串行并行分组
			if !w.isRunning() && w.current != nil {
				txs := make(map[common.Address][]*types.Transaction, len(ev.Txs)) // txs是一个map，key是address，value是tx数组
				for _, tx := range ev.Txs {
					acc, _ := types.Sender(w.current.signer, tx) // Sender函数使用secp256k1椭圆曲线返回从签名(V, R, S)导出的地址，如果导出失败或签名不正确则返回错误
					txs[acc] = append(txs[acc], tx)              // 对应的key(address)加上对应的value(tx)
				}

				// txset := make(map[int][]*types.Transaction) // TODO: 这里实际上调用了分组函数，返回了map[common.Address][]*types.Transactions数据类型，这里防止报错模拟一下

				txset := core.ClassifyTx(ev.Txs, w.current.signer)

				// 获取组数
				var RouNum int = 0
				for _, _ = range txset {
					RouNum++
				}
				// 周期内Tx计数
				err := w.commitTransactions(w.current, txset, nil, RouNum)
				if err != nil {
					return
				}

				// TODO: commit
				BlockHash, err := w.current.state.Commit(true)
				if err != nil {
					fmt.Println("commit函数出现错误")
					return
				}
				fmt.Println(BlockHash) // TODO: 后续可能需要修改

				w.updateSnapshot(w.current) // 更新快照

			} else {

				if w.chainConfig.Clique != nil && w.chainConfig.Clique.Period == 0 {
					w.commitWork(nil, time.Now().Unix())
				}
			}
			w.newTxs.Add(int32(len(ev.Txs)))
		// System stopped
		case <-w.exitCh:
			return
		case <-w.txsSub.Err():
			return
		case <-w.chainHeadSub.Err():
			return
		}
	}
}

// taskLoop is a standalone goroutine to fetch sealing task from the generator and
// push them to consensus engine.
func (w *worker) taskLoop() {
	defer w.wg.Done()
	var (
		stopCh chan struct{}
		prev   common.Hash
	)

	// interrupt aborts the in-flight sealing task.
	interrupt := func() {
		if stopCh != nil {
			close(stopCh)
			stopCh = nil
		}
	}
	for {
		select {
		case task := <-w.taskCh:
			if w.newTaskHook != nil {
				w.newTaskHook(task)
			}
			// Reject duplicate sealing work due to resubmitting.
			sealHash := w.engine.SealHash(task.block.Header())
			if sealHash == prev {
				continue
			}
			// Interrupt previous sealing operation
			interrupt()
			stopCh, prev = make(chan struct{}), sealHash

			if w.skipSealHook != nil && w.skipSealHook(task) {
				continue
			}
			w.pendingMu.Lock()
			w.pendingTasks[sealHash] = task
			w.pendingMu.Unlock()

			if err := w.engine.Seal(w.chain, task.block, w.resultCh, stopCh); err != nil {
				log.Warn("Block sealing failed", "err", err)
				w.pendingMu.Lock()
				delete(w.pendingTasks, sealHash)
				w.pendingMu.Unlock()
			}
		case <-w.exitCh:
			interrupt()
			return
		}
	}
}

// resultLoop is a standalone goroutine to handle sealing result submitting
// and flush relative data to the database.
func (w *worker) resultLoop() {
	defer w.wg.Done()
	for {
		select {
		case block := <-w.resultCh:
			// Short circuit when receiving empty result.
			if block == nil {
				continue
			}
			// Short circuit when receiving duplicate result caused by resubmitting.
			if w.chain.HasBlock(block.Hash(), block.NumberU64()) {
				continue
			}
			var (
				sealhash = w.engine.SealHash(block.Header())
				hash     = block.Hash()
			)
			w.pendingMu.RLock()
			task, exist := w.pendingTasks[sealhash]
			w.pendingMu.RUnlock()
			if !exist {
				log.Error("Block found but no relative pending task", "number", block.Number(), "sealhash", sealhash, "hash", hash)
				continue
			}
			// Different block could share same sealhash, deep copy here to prevent write-write conflict.
			var (
				receipts = make([]*types.Receipt, len(task.receipts))
				logs     []*types.Log
			)
			for i, taskReceipt := range task.receipts {
				receipt := new(types.Receipt)
				receipts[i] = receipt
				*receipt = *taskReceipt

				// add block location fields
				receipt.BlockHash = hash
				receipt.BlockNumber = block.Number()
				receipt.TransactionIndex = uint(i)

				// Update the block hash in all logs since it is now available and not when the
				// receipt/log of individual transactions were created.
				receipt.Logs = make([]*types.Log, len(taskReceipt.Logs))
				for i, taskLog := range taskReceipt.Logs {
					log := new(types.Log)
					receipt.Logs[i] = log
					*log = *taskLog
					log.BlockHash = hash
				}
				logs = append(logs, receipt.Logs...)
			}
			// Commit block and state to database.
			_, err := w.chain.WriteBlockAndSetHead(block, receipts, logs, task.state, true)
			if err != nil {
				log.Error("Failed writing block to chain", "err", err)
				continue
			}
			log.Info("Successfully sealed new block", "number", block.Number(), "sealhash", sealhash, "hash", hash,
				"elapsed", common.PrettyDuration(time.Since(task.createdAt)))

			// Broadcast the block and announce chain insertion event
			w.mux.Post(core.NewMinedBlockEvent{Block: block})

		case <-w.exitCh:
			return
		}
	}
}

// makeEnv creates a new environment for the sealing block.
func (w *worker) makeEnv(parent *types.Header, header *types.Header, coinbase common.Address) (*environment, error) {
	// Retrieve the parent state to execute on top and start a prefetcher for
	// the miner to speed block sealing up a bit.
	state, err := w.chain.StateAt(parent.Root)
	if err != nil {
		return nil, err
	}
	state.StartPrefetcher("miner") // TODO: use here

	// Note the passed coinbase may be different with header.Coinbase.
	env := &environment{
		signer:   types.MakeSigner(w.chainConfig, header.Number, header.Time),
		state:    state,
		coinbase: coinbase,
		header:   header,
	}
	// Keep track of transactions which return errors so they can be removed
	env.tcount = 0
	return env, nil
}

// updateSnapshot updates pending snapshot block, receipts and state.
func (w *worker) updateSnapshot(env *environment) {
	w.snapshotMu.Lock()
	defer w.snapshotMu.Unlock()

	w.snapshotBlock = types.NewBlock(
		env.header,
		env.txs,
		nil,
		env.receipts,
		trie.NewStackTrie(nil),
	)
	w.snapshotReceipts = copyReceipts(env.receipts)
	w.snapshotState = env.state.Copy()
}

func (w *worker) commitTransaction(env *environment, tx *types.Transaction, IsSerial bool) ([]*types.Log, error, RouReturn) {
	var (
		snap = env.state.Snapshot() // 改为针对单个tx的快照保存
		gp   = env.gasPool.Gas()    // todo：??????????????
	)
	fmt.Println(gp) // 用一下gp防止报错

	// todo:实际执行交易函数，IsParallel如果为false表示当前交易需要放入串行队列中
	receipt, err, IsParallel := core.ApplyTransaction(w.chainConfig, w.chain, &env.coinbase, env.gasPool, env.state, env.header, tx, &env.header.GasUsed, *w.chain.GetVMConfig(), IsSerial)
	if err != nil {
		env.state.RevertToSnapshot(snap) // 回滚单个交易状态
		// env.gasPool.SetGas(gp)

		// todo:运行出错的交易
		var roureturn RouReturn
		roureturn.IsParallel = IsParallel
		roureturn.IsSuccess = false
		roureturn.Error = err
		roureturn.TX = tx
		roureturn.Receipt = receipt

		return nil, err, roureturn
	}

	// todo：串行交易的处理
	if !IsParallel {
		// 串行交易
	}

	// todo:正常交易的处理
	var roureturn RouReturn
	roureturn.IsParallel = true
	roureturn.IsSuccess = true
	roureturn.Error = nil
	roureturn.TX = tx
	roureturn.Receipt = receipt

	return receipt.Logs, nil, roureturn
}

// RouReturn todo：线程返回结构体
type RouReturn struct {
	IsParallel bool               // 是否并行执行 false则需要放入串行队列
	IsSuccess  bool               // 是否成功执行 false则说明交易出错
	Error      error              // 如果出错，出错的原因是什么
	TX         *types.Transaction // 交易
	Receipt    *types.Receipt     // 收据
}

type MesReturn struct {
	// 交易队列，存储串行交易
	SerialTxList []*types.Transaction
	// 成功交易数组
	newTxs []*types.Transaction
	// 成功交易收据树
	newReceipt []*types.Receipt
	// logs
	coalescedLogs []*types.Log
}

func (w *worker) commitTransactions(env *environment, txs map[int][]*types.Transaction, interrupt *atomic.Int32, RouNum int) error {
	gasLimit := env.header.GasLimit // 在以太坊中,执行交易需要消耗 gas。为了防止区块中的交易消耗过多资源,导致区块验证时间过长,所以每个区块也设置了 gas limit
	// 区块的 gas limit 是动态调整的,会根据网络实际情况自动调节,以保证交易可以正常处理而不会造成区块膨胀
	if env.gasPool == nil { // 打包交易可用的汽油费
		env.gasPool = new(core.GasPool).AddGas(gasLimit) // 区块汽油费限制
	}
	var coalescedLogs []*types.Log

	// 检查中断信号,如果触发则中止构建
	if interrupt != nil {
		if signal := interrupt.Load(); signal != commitInterruptNone {
			return signalToErr(signal) // TODO: 这是commitTransaction函数唯一返回错误的地方
		}
	}

	env.gasPool.AddGas(999999999999) // 加入无穷大的数，因为txpool已经保证了费用可以支付，因此gaspool在交易时无需判断

	rouchan := make(chan MesReturn, RouNum) // 创建一个带缓冲的通道
	var wg sync.WaitGroup                   // 等待组

	var RouLine []*environment // 存env

	for _, _ = range txs {
		// 每个组都是AccessList不会互相交叉的情况，因此就算创建时间有延迟，也不会影响最后的结果，所以我们把创建过程放在循环里面
		eachENV := env.copy() // 每个交易执行组都有一个独立的env环境
		RouLine = append(RouLine, eachENV)
	}

	fmt.Println("============= len(txs) = ", len(txs)) // ! byd txs里都是空的

	// 建立m+1个交易执行环境
	for i, value := range txs {
		// key是组号(可能没有意义)，value是排好序的交易组
		go w.RouTest(RouLine[i], value, &wg, rouchan, false) // todo:txs应该是每个组，而且这里不能这样写，因为每组传入的是不一样的
	}

	wg.Wait() // 等待并行组执行结束

	for i, _ := range txs {
		// todo:后续更改哈希合并
		RouLine[i].state.Finalise(true)
	}

	// 再次拷贝一份最新的env
	eachENV := env.copy() // 每个交易执行组都有一个独立的env环境

	// todo:串行队列
	var SerialTxList []*types.Transaction

	fmt.Println("=============", len(rouchan)) // !!! 空的

	for v := range rouchan { // 通过通道接收操作获取函数的返回值
		break
		env.txs = append(env.txs, v.newTxs...)                    // 上传tx
		env.receipts = append(env.receipts, v.newReceipt...)      // 上传收据
		SerialTxList = append(SerialTxList, v.SerialTxList...)    // 串行队列
		coalescedLogs = append(coalescedLogs, v.coalescedLogs...) // logs
		if len(rouchan) == 0 {
			fmt.Println("channel is end")
			break
		}
	}

	// 串行组排序
	SortSerialTX(SerialTxList) // 排完序的SingleTxList

	rouchan1 := make(chan MesReturn, RouNum) // 创建一个带缓冲的通道

	// TODO: 执行串行组交易
	go w.RouTest(eachENV, SerialTxList, &wg, rouchan1, true)

	wg.Wait() // 串行组执行结束

	for v := range rouchan1 { // 通过通道接收操作获取函数的返回值
		env.txs = append(env.txs, v.newTxs...)                    // 上传tx
		env.receipts = append(env.receipts, v.newReceipt...)      // 上传收据
		coalescedLogs = append(coalescedLogs, v.coalescedLogs...) // logs
		if len(rouchan1) == 0 {
			fmt.Println("channel is end")
			break
		}
	}

	if !w.isRunning() && len(coalescedLogs) > 0 {
		cpy := make([]*types.Log, len(coalescedLogs))
		for i, l := range coalescedLogs {
			cpy[i] = new(types.Log)
			*cpy[i] = *l
		}
		w.pendingLogsFeed.Send(cpy)
	}

	// TODO: 提交最新状态到内存中
	eachENV.state.Finalise(true)

	return nil
}

// SortSerialTX 串行队列排序方法
func SortSerialTX(SingleTxList []*types.Transaction) {
	// 使用 sort.Slice 排序函数对第二项进行排序
	sort.Slice(SingleTxList, func(i, j int) bool {
		// todo：1 Nonce从小到大
		if SingleTxList[i].Nonce() < SingleTxList[j].Nonce() {
			return true
		} else if SingleTxList[i].Nonce() == SingleTxList[j].Nonce() {
			// todo：2 GasPrice从大到小
			if SingleTxList[i].GasPrice().Cmp(SingleTxList[j].GasPrice()) == 1 {
				return true
			} else if SingleTxList[i].GasPrice().Cmp(SingleTxList[j].GasPrice()) == 0 {
				// todo：3 哈希值从小到大
				if SingleTxList[i].Hash().Less(SingleTxList[j].Hash()) {
					return true
				}
			}
		}
		return false
	})
}

// RouTest todo:MesReturn 是一个通道，专门处理线程处理的返回值
func (w *worker) RouTest(env *environment, txs []*types.Transaction, wg *sync.WaitGroup, RouChan chan MesReturn, IsSerial bool) {
	wg.Add(1)
	defer wg.Done() // 计数器-1

	fmt.Println("RouTest函数开始执行")

	// 新建交易队列，存储串行交易
	var SerialTxList []*types.Transaction
	// 处理成功交易数组
	var newTxs []*types.Transaction
	// 处理成功交易收据树
	var newReceipt []*types.Receipt
	// logs
	var coalescedLogs []*types.Log

	for i := 0; i < len(txs); i++ {
		tx := txs[i]

		if tx == nil {
			fmt.Println("交易为空")
			break // TODO: 为什么要直接退出？
		}

		// 这里的错误可以忽略。这个错误已经在交易池接受交易时检查过了
		from, _ := types.Sender(env.signer, tx)

		// 检查交易是否是重放保护的。如果我们还没到EIP155硬分叉阶段,则开始忽略发送者直到hard fork发生
		if tx.Protected() && !w.chainConfig.IsEIP155(env.header.Number) {
			log.Trace("Ignoring reply protected transaction", "hash", tx.Hash(), "eip155", w.chainConfig.EIP155Block)
			continue
		}

		// 执行交易
		logs, err, rouReturn := w.commitTransaction(env, tx, IsSerial) // todo:rou_return是结构体，记录返回交易

		// 处理正常交易
		if rouReturn.IsSuccess {
			newTxs = append(newTxs, rouReturn.TX)              // 添加处理成功的交易
			newReceipt = append(newReceipt, rouReturn.Receipt) // 添加处理成功的交易的收据
			coalescedLogs = append(coalescedLogs, logs...)     // 添加logs
		} else {
			// TODO: 处理错误交易

			fmt.Println("您提交了一个错误交易")
		}

		// 处理串行交易
		if !rouReturn.IsParallel {
			// 交易是串行的，需要放入串行执行队列
			SerialTxList = append(SerialTxList, rouReturn.TX)
		}

		// 如果交易出现错误，也有可能没有错误，错误处理，判断依据主要是nonce值
		switch {
		case errors.Is(err, core.ErrNonceTooLow): // 如果交易的nonce低于本地链中存在的nonce,则返回ErrNonceTooLow
			// 交易池和矿工之间的新头通知数据竞争,进行移动操作
			log.Trace("Skipping transaction with low nonce", "sender", from, "nonce", tx.Nonce())
			// 实际上不会出现这种情况，交易池给我的已经是Nonce满足的交易

		case errors.Is(err, nil):
			// 一切正常,收集日志并获取同一账户的下一笔交易
			env.tcount++

		default:
			// 该交易被视为无效,因为“nonce太高”的限制,丢弃来自同一发送者的所有后续交易
			log.Debug("Transaction failed, account skipped", "hash", tx.Hash(), "err", err)
			// 实际上不会出现这种情况，交易池给我的已经是Nonce满足的交易
		}
	}

	// todo:处理返回数据
	returnChan := MesReturn{
		SerialTxList:  SerialTxList,
		newTxs:        newTxs,
		newReceipt:    newReceipt,
		coalescedLogs: coalescedLogs,
	}

	// todo:通过管道返回数据
	RouChan <- returnChan

	fmt.Println("这里 routest 函数没有提前返回，顺利执行完了所有功能")

}

// generateParams wraps various of settings for generating sealing task.
type generateParams struct {
	timestamp   uint64            // The timstamp for sealing task
	forceTime   bool              // Flag whether the given timestamp is immutable or not
	parentHash  common.Hash       // Parent block hash, empty means the latest chain head
	coinbase    common.Address    // The fee recipient address for including transaction
	random      common.Hash       // The randomness generated by beacon chain, empty before the merge
	withdrawals types.Withdrawals // List of withdrawals to include in block.
	noTxs       bool              // Flag whether an empty block without any transaction is expected
}

// prepareWork constructs the sealing task according to the given parameters,
// either based on the last chain head or specified parent. In this function
// the pending transactions are not filled yet, only the empty task returned.
func (w *worker) prepareWork(genParams *generateParams) (*environment, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	// Find the parent block for sealing task
	parent := w.chain.CurrentBlock()
	if genParams.parentHash != (common.Hash{}) {
		block := w.chain.GetBlockByHash(genParams.parentHash)
		if block == nil {
			return nil, fmt.Errorf("missing parent")
		}
		parent = block.Header()
	}
	// Sanity check the timestamp correctness, recap the timestamp
	// to parent+1 if the mutation is allowed.
	timestamp := genParams.timestamp
	if parent.Time >= timestamp {
		if genParams.forceTime {
			return nil, fmt.Errorf("invalid timestamp, parent %d given %d", parent.Time, timestamp)
		}
		timestamp = parent.Time + 1
	}
	// Construct the sealing block header.
	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     new(big.Int).Add(parent.Number, common.Big1),
		GasLimit:   core.CalcGasLimit(parent.GasLimit, w.config.GasCeil),
		Time:       timestamp,
		Coinbase:   genParams.coinbase,
	}
	// Set the extra field.
	if len(w.extra) != 0 {
		header.Extra = w.extra
	}
	// Set the randomness field from the beacon chain if it's available.
	if genParams.random != (common.Hash{}) {
		header.MixDigest = genParams.random
	}
	// Set baseFee and GasLimit if we are on an EIP-1559 chain
	if w.chainConfig.IsLondon(header.Number) {
		header.BaseFee = misc.CalcBaseFee(w.chainConfig, parent)
		if !w.chainConfig.IsLondon(parent.Number) {
			parentGasLimit := parent.GasLimit * w.chainConfig.ElasticityMultiplier()
			header.GasLimit = core.CalcGasLimit(parentGasLimit, w.config.GasCeil)
		}
	}
	// Run the consensus preparation with the default or customized consensus engine.
	if err := w.engine.Prepare(w.chain, header); err != nil {
		log.Error("Failed to prepare header for sealing", "err", err)
		return nil, err
	}
	// Could potentially happen if starting to mine in an odd state.
	// Note genParams.coinbase can be different with header.Coinbase
	// since clique algorithm can modify the coinbase field in header.
	env, err := w.makeEnv(parent, header, genParams.coinbase)
	if err != nil {
		log.Error("Failed to create sealing context", "err", err)
		return nil, err
	}
	return env, nil
}

// fillTransactions 从 txpool 检索挂起的交易，并将它们填充到给定的密封块中。交易选择和排序策略可以在将来使用插件进行定制。
func (w *worker) fillTransactions(interrupt *atomic.Int32, env *environment) error {

	fmt.Println("运行 fillTransactions 函数")

	// Split the pending transactions into locals and remotes
	// Fill the block with all available pending transactions.
	pending := w.eth.TxPool().Pending(true)

	localTxs, remoteTxs := make(map[common.Address][]*types.Transaction), pending
	for _, account := range w.eth.TxPool().Locals() {
		if txs := remoteTxs[account]; len(txs) > 0 {
			delete(remoteTxs, account)
			localTxs[account] = txs
		}
	}
	if len(localTxs) > 0 {
		// todo：同样的排序方法
		// txs := types.NewTransactionsByPriceAndNonce(env.signer, localTxs, env.header.BaseFee)

		// todo：localTxs 需要调用分组方法，类型一样，可以直接调用，然后返回分组，这里我们先模拟一个
		// 传入 map[common.Address][]*types.Transaction 传出 map[int][]*types.Transaction
		// txs := make(map[int][]*types.Transaction)

		txs := core.ClassifyTx(env.txs, w.current.signer) // 分组
		// !!! byd传过来的交易就是空的

		// todo:获取组数
		var RouNum int = 0
		for _, _ = range txs {
			RouNum++
		}

		// 直接调用就可以，哦对了还需要处理一下组数，多线程是在函数里面自动完成的
		if err := w.commitTransactions(env, txs, interrupt, RouNum); err != nil { // todo:传入组数
			return err
		}
	}
	if len(remoteTxs) > 0 {
		// todo：同样的排序方法
		// txs := types.NewTransactionsByPriceAndNonce(env.signer, remoteTxs, env.header.BaseFee)

		// todo：remoteTxs 需要调用分组方法，类型一样，可以直接调用，然后返回分组，这里我们先模拟一个
		txs := make(map[int][]*types.Transaction)

		// todo:获取组数
		var RouNum int = 0
		for _, _ = range txs {
			RouNum++
		}
		if err := w.commitTransactions(env, txs, interrupt, RouNum); err != nil {
			return err
		} // todo:传入组数
	}
	return nil
}

// generateWork generates a sealing block based on the given parameters.
func (w *worker) generateWork(params *generateParams) (*types.Block, *big.Int, error) {
	work, err := w.prepareWork(params)
	if err != nil {
		return nil, nil, err
	}
	defer work.discard()

	if !params.noTxs {
		interrupt := new(atomic.Int32)
		timer := time.AfterFunc(w.newpayloadTimeout, func() {
			interrupt.Store(commitInterruptTimeout)
		})
		defer timer.Stop()

		err := w.fillTransactions(interrupt, work)
		if errors.Is(err, errBlockInterruptedByTimeout) {
			log.Warn("Block building is interrupted", "allowance", common.PrettyDuration(w.newpayloadTimeout))
		}
	}
	block, err := w.engine.FinalizeAndAssemble(w.chain, work.header, work.state, work.txs, nil, work.receipts, params.withdrawals)
	if err != nil {
		return nil, nil, err
	}
	return block, totalFees(block, work.receipts), nil
}

// commitWork generates several new sealing tasks based on the parent block
// and submit them to the sealer.
func (w *worker) commitWork(interrupt *atomic.Int32, timestamp int64) {
	// Abort committing if node is still syncing
	if w.syncing.Load() {
		return
	}
	start := time.Now()

	// Set the coinbase if the worker is running or it's required
	var coinbase common.Address
	if w.isRunning() {
		coinbase = w.etherbase()
		if coinbase == (common.Address{}) {
			log.Error("Refusing to mine without etherbase")
			return
		}
	}
	work, err := w.prepareWork(&generateParams{
		timestamp: uint64(timestamp),
		coinbase:  coinbase,
	})
	if err != nil {
		return
	}
	// Fill pending transactions from the txpool into the block.
	err = w.fillTransactions(interrupt, work) // !!!
	switch {
	case err == nil:
		// The entire block is filled, decrease resubmit interval in case
		// of current interval is larger than the user-specified one.
		w.resubmitAdjustCh <- &intervalAdjust{inc: false}

	case errors.Is(err, errBlockInterruptedByRecommit):
		// Notify resubmit loop to increase resubmitting interval if the
		// interruption is due to frequent commits.
		gaslimit := work.header.GasLimit
		ratio := float64(gaslimit-work.gasPool.Gas()) / float64(gaslimit)
		if ratio < 0.1 {
			ratio = 0.1
		}
		w.resubmitAdjustCh <- &intervalAdjust{
			ratio: ratio,
			inc:   true,
		}

	case errors.Is(err, errBlockInterruptedByNewHead):
		// If the block building is interrupted by newhead event, discard it
		// totally. Committing the interrupted block introduces unnecessary
		// delay, and possibly causes miner to mine on the previous head,
		// which could result in higher uncle rate.
		work.discard()
		return
	}
	// Submit the generated block for consensus sealing.
	w.commit(work.copy(), w.fullTaskHook, true, start)

	// Swap out the old work with the new one, terminating any leftover
	// prefetcher processes in the mean time and starting a new one.
	if w.current != nil {
		w.current.discard()
	}
	w.current = work
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
		// Withdrawals are set to nil here, because this is only called in PoW.
		block, err := w.engine.FinalizeAndAssemble(w.chain, env.header, env.state, env.txs, nil, env.receipts, nil)
		if err != nil {
			return err
		}
		// If we're post merge, just ignore
		if !w.isTTDReached(block.Header()) {
			select {
			case w.taskCh <- &task{receipts: env.receipts, state: env.state, block: block, createdAt: time.Now()}:
				fees := totalFees(block, env.receipts)
				feesInEther := new(big.Float).Quo(new(big.Float).SetInt(fees), big.NewFloat(params.Ether))
				log.Info("Commit new sealing work", "number", block.Number(), "sealhash", w.engine.SealHash(block.Header()),
					"txs", env.tcount, "gas", block.GasUsed(), "fees", feesInEther,
					"elapsed", common.PrettyDuration(time.Since(start)))

			case <-w.exitCh:
				log.Info("Worker has exited")
			}
		}
	}
	if update {
		w.updateSnapshot(env)
	}
	return nil
}

// getSealingBlock generates the sealing block based on the given parameters.
// The generation result will be passed back via the given channel no matter
// the generation itself succeeds or not.
func (w *worker) getSealingBlock(parent common.Hash, timestamp uint64, coinbase common.Address, random common.Hash, withdrawals types.Withdrawals, noTxs bool) (*types.Block, *big.Int, error) {
	req := &getWorkReq{
		params: &generateParams{
			timestamp:   timestamp,
			forceTime:   true,
			parentHash:  parent,
			coinbase:    coinbase,
			random:      random,
			withdrawals: withdrawals,
			noTxs:       noTxs,
		},
		result: make(chan *newPayloadResult, 1),
	}
	select {
	case w.getWorkCh <- req:
		result := <-req.result
		if result.err != nil {
			return nil, nil, result.err
		}
		return result.block, result.fees, nil
	case <-w.exitCh:
		return nil, nil, errors.New("miner closed")
	}
}

// isTTDReached returns the indicator if the given block has reached the total
// terminal difficulty for The Merge transition.
func (w *worker) isTTDReached(header *types.Header) bool {
	td, ttd := w.chain.GetTd(header.ParentHash, header.Number.Uint64()-1), w.chain.Config().TerminalTotalDifficulty
	return td != nil && ttd != nil && td.Cmp(ttd) >= 0
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

// totalFees computes total consumed miner fees in Wei. Block transactions and receipts have to have the same order.
func totalFees(block *types.Block, receipts []*types.Receipt) *big.Int {
	feesWei := new(big.Int)
	for i, tx := range block.Transactions() {
		minerFee, _ := tx.EffectiveGasTip(block.BaseFee())
		feesWei.Add(feesWei, new(big.Int).Mul(new(big.Int).SetUint64(receipts[i].GasUsed), minerFee))
	}
	return feesWei
}

// signalToErr converts the interruption signal to a concrete error type for return.
// The given signal must be a valid interruption signal.
func signalToErr(signal int32) error {
	switch signal {
	case commitInterruptNewHead:
		return errBlockInterruptedByNewHead
	case commitInterruptResubmit:
		return errBlockInterruptedByRecommit
	case commitInterruptTimeout:
		return errBlockInterruptedByTimeout
	default:
		panic(fmt.Errorf("undefined signal %d", signal))
	}
}
