// Copyright 2014 The go-ethereum Authors
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

package core

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/params"
	rpcClient "github.com/tendermint/tendermint/rpc/client"
	"gopkg.in/karalabe/cookiejar.v2/collections/prque"
	emtConfig "github.com/dora/ultron/node/config"
)

var (
	// ErrInvalidSender is returned if the transaction contains an invalid signature.
	ErrInvalidSender = errors.New("invalid sender")

	// ErrNonceTooLow is returned if the nonce of a transaction is lower than the
	// one present in the local chain.
	ErrNonceTooLow = errors.New("nonce too low")

	// ErrUnderpriced is returned if a transaction's gas price is below the minimum
	// configured for the transaction pool.
	ErrUnderpriced = errors.New("transaction underpriced")

	// ErrReplaceUnderpriced is returned if a transaction is attempted to be replaced
	// with a different one without the required price bump.
	ErrReplaceUnderpriced = errors.New("replacement transaction underpriced")

	// ErrInsufficientFunds is returned if the total cost of executing a transaction
	// is higher than the balance of the user's account.
	ErrInsufficientFunds = errors.New("insufficient funds for gas * price + value")

	// ErrIntrinsicGas is returned if the transaction is specified to use less gas
	// than required to start the invocation.
	ErrIntrinsicGas = errors.New("intrinsic gas too low")

	// ErrGasLimit is returned if a transaction's requested gas limit exceeds the
	// maximum allowance of the current block.
	ErrGasLimit = errors.New("exceeds block gas limit")

	// ErrNegativeValue is a sanity error to ensure noone is able to specify a
	// transaction with a negative value.
	ErrNegativeValue = errors.New("negative value")

	// ErrOversizedData is returned if the input data of a transaction is greater
	// than some meaningful limit a user might use. This is not a consensus error
	// making the transaction invalid, rather a DOS protection.
	ErrOversizedData = errors.New("oversized data")

	// ErrNonceNoReplace can not replace pending tx of the same nonce
	// as pending tx has already been broadcast to tendermint
	ErrNonceNotReplaced = errors.New("can not replace pending nonce")
	// ErrBroadcastTX error occured when broadcasting tx to tendermint
	ErrBroadcastTX = errors.New("failed to broadcast tx to tendermint")
)

var (
	evictionInterval    = time.Minute     // Time interval to check for evictable transactions
	statsReportInterval = 8 * time.Second // Time interval to report transaction pool stats
)

var (
	// Metrics for the pending pool
	pendingDiscardCounter   = metrics.NewCounter("txpool/pending/discard")
	pendingReplaceCounter   = metrics.NewCounter("txpool/pending/replace")
	pendingRateLimitCounter = metrics.NewCounter("txpool/pending/ratelimit") // Dropped due to rate limiting
	pendingNofundsCounter   = metrics.NewCounter("txpool/pending/nofunds")   // Dropped due to out-of-funds

	// Metrics for the queued pool
	queuedDiscardCounter   = metrics.NewCounter("txpool/queued/discard")
	queuedReplaceCounter   = metrics.NewCounter("txpool/queued/replace")
	queuedRateLimitCounter = metrics.NewCounter("txpool/queued/ratelimit") // Dropped due to rate limiting
	queuedNofundsCounter   = metrics.NewCounter("txpool/queued/nofunds")   // Dropped due to out-of-funds

	// General tx metrics
	invalidTxCounter = metrics.NewCounter("txpool/invalid")
	//underpricedTxCounter = metrics.NewCounter("txpool/underpriced")
)

var repeatTxTest = false

type stateFn func() (*state.StateDB, error)

// TxPoolConfig are the configuration parameters of the transaction pool.
type TxPoolConfig struct {
	NoLocals bool // Whether local transaction handling should be disabled

	PriceLimit uint64 // Minimum gas price to enforce for acceptance into the pool
	PriceBump  uint64 // Minimum price bump percentage to replace an already existing transaction (nonce)

	AccountSlots uint64 // Minimum number of executable transaction slots guaranteed per account
	GlobalSlots  uint64 // Maximum number of executable transaction slots for all accounts
	AccountQueue uint64 // Maximum number of non-executable transaction slots permitted per account
	GlobalQueue  uint64 // Maximum number of non-executable transaction slots for all accounts

	Lifetime time.Duration // Maximum amount of time non-executable transaction are queued
}

// DefaultTxPoolConfig contains the default configurations for the transaction
// pool.
var DefaultTxPoolConfig = TxPoolConfig{
	PriceLimit: 1,
	PriceBump:  10,

	AccountSlots: 16,
	GlobalSlots:  32768,
	AccountQueue: 64,
	GlobalQueue:  4096,

	Lifetime: 3 * time.Hour,
}

// sanitize checks the provided user configurations and changes anything that's
// unreasonable or unworkable.
func (config *TxPoolConfig) sanitize() TxPoolConfig {
	conf := *config
	if conf.PriceLimit < 1 {
		log.Warn("Sanitizing invalid txpool price limit", "provided", conf.PriceLimit, "updated", DefaultTxPoolConfig.PriceLimit)
		conf.PriceLimit = DefaultTxPoolConfig.PriceLimit
	}
	if conf.PriceBump < 1 {
		log.Warn("Sanitizing invalid txpool price bump", "provided", conf.PriceBump, "updated", DefaultTxPoolConfig.PriceBump)
		conf.PriceBump = DefaultTxPoolConfig.PriceBump
	}
	return conf
}

// TxPool contains all currently known transactions. Transactions
// enter the pool when they are received from the network or submitted
// locally. They exit the pool when they are included in the blockchain.
//
// The pool separates processable transactions (which can be applied to the
// current state) and future transactions. Transactions move between those
// two states over time as they are received and processed.
type TxPool struct {
	config       TxPoolConfig
	chainconfig  *params.ChainConfig
	currentState stateFn // The state function which will allow us to do some pre checks
	pendingState *state.ManagedState
	gasLimit     func() *big.Int // The current gas limit function callback
	gasPrice     *big.Int
	eventMux     *event.TypeMux
	events       *event.TypeMuxSubscription
	//locals       *accountSet
	signer types.Signer
	mu     sync.RWMutex

	pending 		map[common.Address]*txList         	// All currently processable transactions
	pendingVolume	uint64						   		// Item count in pending
	queue   		map[common.Address]*txList         	// Queued but non-processable transactions
	queueVolume		uint64								// Item count in queue
	beats   		map[common.Address]time.Time       	// Last heartbeat from each known account
	all     		map[common.Hash]*types.Transaction 	// All transactions to allow lookups
	//priced  *txPricedList                      // All transactions sorted by price

	wg   sync.WaitGroup // for shutdown sync
	quit chan struct{}

	homestead bool

	tmClient *rpcClient.Local
}

// NewTxPool creates a new transaction pool to gather, sort and filter inbound
// trnsactions from the network.
func NewTxPool(config TxPoolConfig, chainconfig *params.ChainConfig, eventMux *event.TypeMux, currentStateFn stateFn, gasLimitFn func() *big.Int) *TxPool {
	// Sanitize the input to ensure no vulnerable gas prices are set
	config = (&config).sanitize()

	// Create the transaction pool with its initial settings
	pool := &TxPool{
		config:       config,
		chainconfig:  chainconfig,
		signer:       types.NewEIP155Signer(chainconfig.ChainId),
		pending:      make(map[common.Address]*txList),
		queue:        make(map[common.Address]*txList),
		beats:        make(map[common.Address]time.Time),
		all:          make(map[common.Hash]*types.Transaction),
		eventMux:     eventMux,
		currentState: currentStateFn,
		gasLimit:     gasLimitFn,
		gasPrice:     new(big.Int).SetUint64(config.PriceLimit),
		pendingState: nil,
		events:       eventMux.Subscribe(ChainHeadEvent{}, RemovedTransactionEvent{}),
		quit:         make(chan struct{}),
		tmClient:     nil,
	}
	//pool.locals = newAccountSet(pool.signer)
	pool.resetState()

	// Start the various events loops and return
	pool.wg.Add(2)
	go pool.eventLoop()
	go pool.expirationLoop()

	testConfig, _ := emtConfig.ParseConfig()
	if (testConfig != nil && testConfig.TestConfig.RepeatTxTest) {
		repeatTxTest = true
	}

	return pool
}

func (pool *TxPool) eventLoop() {
	defer pool.wg.Done()

	// Start a ticker and keep track of interesting pool stats to report
	var prevPending, prevQueued int

	report := time.NewTicker(statsReportInterval)
	defer report.Stop()

	// Track chain events. When a chain events occurs (new chain canon block)
	// we need to know the new state. The new state will help us determine
	// the nonces in the managed state
	for {
		select {
		// Handle any events fired by the system
		case ev, ok := <-pool.events.Chan():
			if !ok {
				return
			}
			switch ev := ev.Data.(type) {
			case ChainHeadEvent:
				pool.mu.Lock()
				if ev.Block != nil {
					if pool.chainconfig.IsHomestead(ev.Block.Number()) {
						pool.homestead = true
					}
				}
				pool.resetState()
				pool.mu.Unlock()

			case RemovedTransactionEvent:
				pool.addTxs(ev.Txs, false)
			}

		// Handle stats reporting ticks
		case <-report.C:
			pool.mu.RLock()
			pending, queued := pool.stats()
			pool.mu.RUnlock()

			if pending != prevPending || queued != prevQueued {
				log.Debug("Transaction pool status report", "executable", pending, "queued", queued)
				prevPending, prevQueued = pending, queued
			}
		}
	}
}

func (pool *TxPool) OnChainHeadEvent() {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	pool.resetState()
}

func (pool *TxPool) resetState() {
	currentState, err := pool.currentState()
	if err != nil {
		log.Error("Failed reset txpool state", "err", err)
		return
	}
	pool.pendingState = state.ManageState(currentState)

	// validate the pool of pending transactions, this will remove
	// any transactions that have been included in the block or
	// have been invalidated because of another transaction (e.g.
	// higher gas price)
	fmt.Println("**********Before delete all size", len(pool.all), "pending", len(pool.pending), 
		"queue", len(pool.queue))
	pool.demoteUnexecutables(currentState)
	fmt.Println("**********After delete all size", len(pool.all), "pending", len(pool.pending), 
		"queue", len(pool.queue))

	// Update all accounts to the latest known pending nonce
	for addr, list := range pool.pending {
		txs := list.Flatten() // Heavy but will be cached and is needed by the miner anyway
		pool.pendingState.SetNonce(addr, txs[len(txs)-1].Nonce()+1)
	}
	// Check the queue and move transactions over to the pending if possible
	// or remove those that have become invalid
	pool.promoteExecutables(currentState, nil)
	fmt.Println("txpool reset state done")
}

// Stop terminates the transaction pool.
func (pool *TxPool) Stop() {
	pool.events.Unsubscribe()
	close(pool.quit)
	pool.wg.Wait()

	log.Info("Transaction pool stopped")
}

// Signer terminates the transaction pool.
func (pool *TxPool) Signer() types.Signer {
	return pool.signer
}

// GasPrice returns the current gas price enforced by the transaction pool.
func (pool *TxPool) GasPrice() *big.Int {
	pool.mu.RLock()
	defer pool.mu.RUnlock()

	return new(big.Int).Set(pool.gasPrice)
}

// SetGasPrice updates the minimum price required by the transaction pool for a
// new transaction, and drops all transactions below this threshold.
func (pool *TxPool) SetGasPrice(price *big.Int) {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	pool.gasPrice = price
	// TODO: Nothing to do as there is no pricedList
	//for _, tx := range pool.priced.Cap(price, pool.locals) {
	//	pool.removeTx(tx.Hash())
	//}
	log.Info("Transaction pool price threshold updated", "price", price)
}

// State returns the virtual managed state of the transaction pool.
func (pool *TxPool) State() *state.ManagedState {
	pool.mu.RLock()
	defer pool.mu.RUnlock()

	return pool.pendingState
}

// Stats retrieves the current pool stats, namely the number of pending and the
// number of queued (non-executable) transactions.
func (pool *TxPool) Stats() (int, int) {
	pool.mu.RLock()
	defer pool.mu.RUnlock()

	return pool.stats()
}

// stats retrieves the current pool stats, namely the number of pending and the
// number of queued (non-executable) transactions.
func (pool *TxPool) stats() (int, int) {
	pending := 0
	for _, list := range pool.pending {
		pending += list.Len()
	}
	queued := 0
	for _, list := range pool.queue {
		queued += list.Len()
	}
	return pending, queued
}

// Content retrieves the data content of the transaction pool, returning all the
// pending as well as queued transactions, grouped by account and sorted by nonce.
func (pool *TxPool) Content() (map[common.Address]types.Transactions, map[common.Address]types.Transactions) {
	pool.mu.RLock()
	defer pool.mu.RUnlock()

	pending := make(map[common.Address]types.Transactions)
	for addr, list := range pool.pending {
		pending[addr] = list.Flatten()
	}
	queued := make(map[common.Address]types.Transactions)
	for addr, list := range pool.queue {
		queued[addr] = list.Flatten()
	}
	return pending, queued
}

// Pending retrieves all currently processable transactions, groupped by origin
// account and sorted by nonce. The returned transaction set is a copy and can be
// freely modified by calling code.
func (pool *TxPool) Pending() (map[common.Address]types.Transactions, error) {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	pending := make(map[common.Address]types.Transactions)
	for addr, list := range pool.pending {
		pending[addr] = list.Flatten()
	}
	return pending, nil
}

// validateTx checks whether a transaction is valid according to the consensus
// rules and adheres to some heuristic limits of the local node (price and size).
func (pool *TxPool) validateTx(tx *types.Transaction, local bool) error {
	// Heuristic limit, reject transactions over 32KB to prevent DOS attacks
	if tx.Size() > 32*1024 {
		return ErrOversizedData
	}
	// Transactions can't be negative. This may never happen using RLP decoded
	// transactions but may occur if you create a transaction using the RPC.
	if tx.Value().Sign() < 0 {
		return ErrNegativeValue
	}
	// Ensure the transaction doesn't exceed the current block limit gas.
	if pool.gasLimit().Cmp(tx.Gas()) < 0 {
		return ErrGasLimit
	}
	// Make sure the transaction is signed properly
	// from, err := types.Sender(pool.signer, tx)
	from, err := tx.From(pool.signer, true)
	if err != nil {
		return ErrInvalidSender
	}
	// Drop non-local transactions under our own minimal accepted gas price
	//local = local || pool.locals.contains(from) // account may be local even if the transaction arrived from the network
	//if !local && pool.gasPrice.Cmp(tx.GasPrice()) > 0 {
	//	return ErrUnderpriced
	//}
	// Ensure the transaction adheres to nonce ordering
	currentState, err := pool.currentState()
	if err != nil {
		return err
	}
	if (!repeatTxTest) {
		if currentState.GetNonce(from) > tx.Nonce() {
			return ErrNonceTooLow
		}
	}
	// Transactor should have enough funds to cover the costs
	// cost == V + GP * GL
	if currentState.GetBalance(from).Cmp(tx.Cost()) < 0 {
		fmt.Println("Discarding low balance transaction")
		return ErrInsufficientFunds
	}
	intrGas := IntrinsicGas(tx.Data(), tx.To() == nil, pool.homestead)
	if tx.Gas().Cmp(intrGas) < 0 {
		return ErrIntrinsicGas
	}
	return nil
}

// add validates a transaction and inserts it into the non-executable queue for
// later pending promotion and execution. If the transaction is a replacement for
// an already pending or queued one, it overwrites the previous and returns this
// so outer code doesn't uselessly call promote.
//
// If a newly added transaction is marked as local, its sending account will be
// whitelisted, preventing any associated transaction from being dropped out of
// the pool due to pricing constraints.
func (pool *TxPool) add(tx *types.Transaction, local bool) (bool, error) {
	// If the transaction fails basic validation, discard it
	hash := tx.Hash()
	if err := pool.validateTx(tx, local); err != nil {
		log.Trace("Discarding invalid transaction", "hash", hash, "err", err)
		invalidTxCounter.Inc(1)
		return false, err
	}
	
	pool.mu.Lock()
	defer pool.mu.Unlock()
	// If the transaction is already known, discard it
	if pool.all[hash] != nil {
		fmt.Println("Discarding already known transaction", "hash", hash)
		log.Trace("Discarding already known transaction", "hash", hash)
		return false, fmt.Errorf("known transaction: %x", hash)
	}

	// If the transaction pool is full, discard underpriced transactions
	if uint64(len(pool.all)) >= pool.config.GlobalSlots+pool.config.GlobalQueue {
		log.Trace("Discarding transaction as there is no room for it", "hash", hash)
		return false, fmt.Errorf("Discarding transaction as there is no room for it: %x", hash)
	}
	// If the transaction is replacing an already pending one, return error
	// from, _ := types.Sender(pool.signer, tx) // already validated
	from, _ := tx.From(pool.signer, false)
	if list := pool.pending[from]; list != nil && list.Overlaps(tx) {
		// Nonce already pending, can not be replaced
		return false, ErrNonceNotReplaced
	}
	// New transaction isn't replacing a pending one, push into queue and potentially mark local
	replace, err := pool.enqueueTx(hash, tx)
	if err != nil {
		return false, err
	}
	//if local {
	//	pool.locals.add(from)
	//}
	log.Trace("Pooled new future transaction", "hash", hash, "from", from, "to", tx.To())
	return replace, nil
}

// enqueueTx inserts a new transaction into the non-executable transaction queue.
//
// Note, this method assumes the pool lock is held!
func (pool *TxPool) enqueueTx(hash common.Hash, tx *types.Transaction) (bool, error) {
	// Try to insert the transaction into the future queue
	// from, _ := types.Sender(pool.signer, tx) // already validated
	from, _ := tx.From(pool.signer, false) // validate from in execution thread
	if pool.queue[from] == nil {
		pool.queue[from] = newTxList(false)
	}
	inserted, old := pool.queue[from].Add(tx, pool.config.PriceBump)
	if !inserted {
		// An older transaction was better, discard this
		queuedDiscardCounter.Inc(1)
		return false, ErrReplaceUnderpriced
	}
	// Discard any previous transaction and mark this
	if old != nil {
		delete(pool.all, old.Hash())
		queuedReplaceCounter.Inc(1)
	} else {
		pool.queueVolume++
	}
	pool.all[hash] = tx
	return old != nil, nil
}

// promoteTx adds a transaction to the pending (processable) list of transactions.
//
// Note, this method assumes the pool lock is held!
func (pool *TxPool) promoteTx(addr common.Address, hash common.Hash, tx *types.Transaction) error {
	// Try to insert the transaction into the pending queue
	if pool.pending[addr] == nil {
		pool.pending[addr] = newTxList(true)
	}
	list := pool.pending[addr]

	inserted, old := list.Add(tx, pool.config.PriceBump)
	// 插入失败直接删除
	if !inserted {
		// An older transaction was better, discard this
		delete(pool.all, hash)

		pendingDiscardCounter.Inc(1)
		return nil
	}
	// Otherwise discard any previous transaction and mark this
	// 插入替换，删除旧的
	if old != nil {
		delete(pool.all, old.Hash())

		pendingReplaceCounter.Inc(1)
	} else {
		pool.pendingVolume++
	}
	// Failsafe to work around direct pending inserts (tests)
	if pool.all[hash] == nil {
		pool.all[hash] = tx
	}
	// Set the potentially new pending nonce and notify any subsystems of the new tx
	pool.beats[addr] = time.Now()
	pool.pendingState.SetNonce(addr, tx.Nonce()+1)

	if pool.tmClient != nil {
		return pool.broadcastTx(tx)
	}
	pool.eventMux.Post(TxPreEvent{tx})
	return nil
}

// AddLocal enqueues a single transaction into the pool if it is valid, marking
// the sender as a local one in the mean time, ensuring it goes around the local
// pricing constraints.
func (pool *TxPool) AddLocal(tx *types.Transaction) error {
	return pool.addTx(tx, !pool.config.NoLocals)
}

// AddRemote enqueues a single transaction into the pool if it is valid. If the
// sender is not among the locally tracked ones, full pricing constraints will
// apply.
func (pool *TxPool) AddRemote(tx *types.Transaction) error {
	return pool.addTx(tx, false)
}

// AddLocals enqueues a batch of transactions into the pool if they are valid,
// marking the senders as a local ones in the mean time, ensuring they go around
// the local pricing constraints.
func (pool *TxPool) AddLocals(txs []*types.Transaction) error {
	return pool.addTxs(txs, !pool.config.NoLocals)
}

// AddRemotes enqueues a batch of transactions into the pool if they are valid.
// If the senders are not among the locally tracked ones, full pricing constraints
// will apply.
func (pool *TxPool) AddRemotes(txs []*types.Transaction) error {
	return pool.addTxs(txs, false)
}

// addTx enqueues a single transaction into the pool if it is valid.
func (pool *TxPool) addTx(tx *types.Transaction, local bool) error {
	// Try to inject the transaction and update any state
	replace, err := pool.add(tx, local)
	if err != nil {
		return err
	}
	// If we added a new transaction, run promotion checks and return
	// 如果是新增的tx，需要进行检查是否有可能提升txs
	// 如果是替换的则什么都不做
	// TODO: 如果替换的是Pending中的tx，这种tx如何处理，是否可以直接报错？

	if !replace {
		state, err := pool.currentState()
		if err != nil {
			return err
		}
		pool.mu.Lock()
		defer pool.mu.Unlock()
		// from, _ := types.Sender(pool.signer, tx) // already validated
		from, _ := tx.From(pool.signer, false) // already validated
		return pool.promoteExecutables(state, []common.Address{from})
	}
	return nil
}

// addTxs attempts to queue a batch of transactions if they are valid.
func (pool *TxPool) addTxs(txs []*types.Transaction, local bool) error {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	// Add the batch of transaction, tracking the accepted ones
	dirty := make(map[common.Address]struct{})
	for _, tx := range txs {
		if replace, err := pool.add(tx, local); err == nil {
			if !replace {
				from, _ := tx.From(pool.signer, false) // already validated
				dirty[from] = struct{}{}
			}
		}
	}
	// Only reprocess the internal state if something was actually added
	if len(dirty) > 0 {
		state, err := pool.currentState()
		if err != nil {
			return err
		}
		addrs := make([]common.Address, 0, len(dirty))
		for addr, _ := range dirty {
			addrs = append(addrs, addr)
		}
		pool.promoteExecutables(state, addrs)
	}
	return nil
}

// Get returns a transaction if it is contained in the pool
// and nil otherwise.
func (pool *TxPool) Get(hash common.Hash) *types.Transaction {
	pool.mu.RLock()
	defer pool.mu.RUnlock()

	return pool.all[hash]
}

// Remove removes the transaction with the given hash from the pool.
func (pool *TxPool) Remove(hash common.Hash) {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	pool.removeTx(hash)
}

// RemoveBatch removes all given transactions from the pool.
func (pool *TxPool) RemoveBatch(txs types.Transactions) {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	for _, tx := range txs {
		pool.removeTx(tx.Hash())
	}
}

// removeTx removes a single transaction from the queue, moving all subsequent
// transactions back to the future queue.
func (pool *TxPool) removeTx(hash common.Hash) {
	// Fetch the transaction we wish to delete
	tx, ok := pool.all[hash]
	if !ok {
		return
	}
	addr, _ := tx.From(pool.signer, false) // already validated during insertion

	// Remove it from the list of known transactions
	delete(pool.all, hash)

	// Remove the transaction from the pending lists and reset the account nonce
	if pending := pool.pending[addr]; pending != nil {
		if removed, invalids := pending.Remove(tx); removed {
			pool.pendingVolume--
			// If no more transactions are left, remove the list
			if pending.Empty() {
				delete(pool.pending, addr)
				delete(pool.beats, addr)
			} else {
				// Otherwise postpone any invalidated transactions
				for _, tx := range invalids {
					pool.enqueueTx(tx.Hash(), tx)
				}
			}
			// Update the account nonce if needed
			if nonce := tx.Nonce(); pool.pendingState.GetNonce(addr) > nonce {
				pool.pendingState.SetNonce(addr, nonce)
			}
			return
		}
	}
	// Transaction is in the future queue
	if future := pool.queue[addr]; future != nil {
		future.Remove(tx)
		pool.queueVolume--
		if future.Empty() {
			delete(pool.queue, addr)
		}
	}
}

// promoteExecutables moves transactions that have become processable from the
// future queue to the set of pending transactions. During this process, all
// invalidated transactions (low nonce, low balance) are deleted.
func (pool *TxPool) promoteExecutables(state *state.StateDB, accounts []common.Address) error {
	gaslimit := pool.gasLimit()

	// Gather all the accounts potentially needing updates
	if accounts == nil {
		accounts = make([]common.Address, 0, len(pool.queue))
		for addr, _ := range pool.queue {
			accounts = append(accounts, addr)
		}
	}
	// Iterate over all accounts and promote any executable transactions
	for _, addr := range accounts {
		list := pool.queue[addr]
		if list == nil {
			continue // Just in case someone calls with a non existing account
		}
		// Drop all transactions that are deemed too old (low nonce)
		// 删除nonce值太低txs
		if (!repeatTxTest) {
			for _, tx := range list.Forward(state.GetNonce(addr)) {
				hash := tx.Hash()
				log.Trace("Removed old queued transaction", "hash", hash)
				delete(pool.all, hash)
				pool.queueVolume--
			}
		}
		// Drop all transactions that are too costly (low balance or out of gas)
		// 删除花费太高的txs
		drops, _ := list.Filter(state.GetBalance(addr), gaslimit)
		for _, tx := range drops {
			hash := tx.Hash()
			log.Trace("Removed unpayable queued transaction", "hash", hash)
			delete(pool.all, hash)
			queuedNofundsCounter.Inc(1)
			pool.queueVolume--
		}
		// Gather all executable transactions and promote them
		// TODO: 区分state与pendingState中的nonce值
		startNonce := pool.pendingState.GetNonce(addr)
		if (repeatTxTest) {
			startNonce = (uint64)(9999999999999999999)
		}
		for _, tx := range list.Ready(startNonce) {
			hash := tx.Hash()
			log.Trace("Promoting queued transaction", "hash", hash)
			// TODO: 剩余的tx怎么处理？？？
			if err := pool.promoteTx(addr, hash, tx); err != nil {
				return err
			}
			pool.queueVolume--
		}
		// Drop all transactions over the allowed limit
		//if !pool.locals.contains(addr) {
		//	for _, tx := range list.Cap(int(pool.config.AccountQueue)) {
		//		hash := tx.Hash()
		//		delete(pool.all, hash)
		//		queuedRateLimitCounter.Inc(1)
		//		log.Trace("Removed cap-exceeding queued transaction", "hash", hash)
		//	}
		//}
		// Delete the entire queue entry if it became empty.
		if list.Empty() {
			delete(pool.queue, addr)
		}
	}

	// ===== 提升之后对阈值的检查 =======
	// TODO: 前面做了拦截，这里的两个逻辑是不是多余了？
	// If the pending limit is overflown, start equalizing allowances
	// 首先对pending的检查
	pending := pool.pendingVolume
	// for _, list := range pool.pending {
	// 	pending += uint64(list.Len())
	// }
	if pending > pool.config.GlobalSlots {
		pendingBeforeCap := pending
		// Assemble a spam order to penalize large transactors first
		spammers := prque.New()
		for addr, list := range pool.pending {
			// Only evict transactions from high rollers
			// if !pool.locals.contains(addr) && uint64(list.Len()) > pool.config.AccountSlots {
			if uint64(list.Len()) > pool.config.AccountSlots {
				spammers.Push(addr, float32(list.Len()))
			}
		}
		// Gradually drop transactions from offenders
		offenders := []common.Address{}
		for pending > pool.config.GlobalSlots && !spammers.Empty() {
			// Retrieve the next offender if not local address
			offender, _ := spammers.Pop()
			offenders = append(offenders, offender.(common.Address))

			// Equalize balances until all the same or below threshold
			if len(offenders) > 1 {
				// Calculate the equalization threshold for all current offenders
				threshold := pool.pending[offender.(common.Address)].Len()

				// Iteratively reduce all offenders until below limit or threshold reached
				for pending > pool.config.GlobalSlots && pool.pending[offenders[len(offenders)-2]].Len() > threshold {
					for i := 0; i < len(offenders)-1; i++ {
						list := pool.pending[offenders[i]]
						for _, tx := range list.Cap(list.Len() - 1) {
							// Drop the transaction from the global pools too
							hash := tx.Hash()
							delete(pool.all, hash)

							pool.pendingVolume--
							// Update the account nonce to the dropped transaction
							if nonce := tx.Nonce(); pool.pendingState.GetNonce(offenders[i]) > nonce {
								pool.pendingState.SetNonce(offenders[i], nonce)
							}
							log.Trace("Removed fairness-exceeding pending transaction", "hash", hash)
						}
						pending--
					}
				}
			}
		}
		// If still above threshold, reduce to limit or min allowance
		if pending > pool.config.GlobalSlots && len(offenders) > 0 {
			for pending > pool.config.GlobalSlots && uint64(pool.pending[offenders[len(offenders)-1]].Len()) > pool.config.AccountSlots {
				for _, addr := range offenders {
					list := pool.pending[addr]
					for _, tx := range list.Cap(list.Len() - 1) {
						// Drop the transaction from the global pools too
						hash := tx.Hash()
						delete(pool.all, hash)

						pool.pendingVolume--
						// Update the account nonce to the dropped transaction
						if nonce := tx.Nonce(); pool.pendingState.GetNonce(addr) > nonce {
							pool.pendingState.SetNonce(addr, nonce)
						}
						log.Trace("Removed fairness-exceeding pending transaction", "hash", hash)
					}
					pending--
				}
			}
		}
		pendingRateLimitCounter.Inc(int64(pendingBeforeCap - pending))
	}
	// If we've queued more transactions than the hard limit, drop oldest ones
	// 然后对future queue的检查
	queued := pool.queueVolume
	// for _, list := range pool.queue {
	// 	queued += uint64(list.Len())
	// }
	if queued > pool.config.GlobalQueue {
		// Sort all accounts with queued transactions by heartbeat
		addresses := make(addresssByHeartbeat, 0, len(pool.queue))
		for addr := range pool.queue {
			// if !pool.locals.contains(addr) { // don't drop locals
			// 	addresses = append(addresses, addressByHeartbeat{addr, pool.beats[addr]})
			// }
			addresses = append(addresses, addressByHeartbeat{addr, pool.beats[addr]})
		}
		sort.Sort(addresses)

		// Drop transactions until the total is below the limit or only locals remain
		for drop := queued - pool.config.GlobalQueue; drop > 0 && len(addresses) > 0; {
			addr := addresses[len(addresses)-1]
			list := pool.queue[addr.address]

			addresses = addresses[:len(addresses)-1]

			// Drop all transactions if they are less than the overflow
			if size := uint64(list.Len()); size <= drop {
				for _, tx := range list.Flatten() {
					pool.removeTx(tx.Hash())
				}
				drop -= size
				queuedRateLimitCounter.Inc(int64(size))
				continue
			}
			// Otherwise drop only last few transactions
			txs := list.Flatten()
			for i := len(txs) - 1; i >= 0 && drop > 0; i-- {
				pool.removeTx(txs[i].Hash())
				drop--
				queuedRateLimitCounter.Inc(1)
			}
		}
	}
	return nil
}

// demoteUnexecutables removes invalid and processed transactions from the pools
// executable/pending queue and any subsequent transactions that become unexecutable
// are moved back into the future queue.
func (pool *TxPool) demoteUnexecutables(state *state.StateDB) {
	gaslimit := pool.gasLimit()

	// Iterate over all accounts and demote any non-executable transactions
	for addr, list := range pool.pending {
		nonce := state.GetNonce(addr)

		// Drop all transactions that are deemed too old (low nonce)
		endNonce := nonce
		if (repeatTxTest) {
			endNonce = nonce + 2	// TODO: there is an issue that nonce haven't added after applyTransaction
		}
		for _, tx := range list.Forward(endNonce) {
			hash := tx.Hash()
			log.Trace("Removed old pending transaction", "hash", hash)
			delete(pool.all, hash)
			pool.pendingVolume--
		}
		// Drop all transactions that are too costly (low balance or out of gas), and queue any invalids back for later
		drops, invalids := list.Filter(state.GetBalance(addr), gaslimit)
		for _, tx := range drops {
			hash := tx.Hash()
			log.Trace("Removed unpayable pending transaction", "hash", hash)
			delete(pool.all, hash)
			pendingNofundsCounter.Inc(1)
			pool.pendingVolume--
		}
		for _, tx := range invalids {
			hash := tx.Hash()
			log.Trace("Demoting pending transaction", "hash", hash)
			pool.enqueueTx(hash, tx)
		}
		// Delete the entire queue entry if it became empty.
		if list.Empty() {
			delete(pool.pending, addr)
			delete(pool.beats, addr)
		}
	}
}

// expirationLoop is a loop that periodically iterates over all accounts with
// queued transactions and drop all that have been inactive for a prolonged amount
// of time.
func (pool *TxPool) expirationLoop() {
	defer pool.wg.Done()

	evict := time.NewTicker(evictionInterval)
	defer evict.Stop()

	for {
		select {
		case <-evict.C:
			pool.mu.Lock()
			for addr := range pool.queue {
				// Skip local transactions from the eviction mechanism
				//if pool.locals.contains(addr) {
				//	continue
				//}
				continue
				// Any non-locals old enough should be removed
				if time.Since(pool.beats[addr]) > pool.config.Lifetime {
					for _, tx := range pool.queue[addr].Flatten() {
						pool.removeTx(tx.Hash())
					}
				}
			}
			pool.mu.Unlock()

		case <-pool.quit:
			return
		}
	}
}

func (pool *TxPool) SetTMClient(client *rpcClient.Local) {
	pool.tmClient = client
}

func (pool *TxPool) broadcastTx(tx *types.Transaction) error {
	buf := new(bytes.Buffer)
	if err := tx.EncodeRLP(buf); err != nil {
		return err
	}
	result, err := pool.tmClient.BroadcastTxSync(buf.Bytes(), 1)
	if err != nil {
		log.Trace("Broadcast error", "err", err)
		return ErrBroadcastTX
	}

	if result.Code != uint32(0) {
		pool.removeTx(tx.Hash())
		fmt.Println(result)
		return errors.New(result.Log)
	}
	return nil
}

// addressByHeartbeat is an account address tagged with its last activity timestamp.
type addressByHeartbeat struct {
	address   common.Address
	heartbeat time.Time
}

type addresssByHeartbeat []addressByHeartbeat

func (a addresssByHeartbeat) Len() int           { return len(a) }
func (a addresssByHeartbeat) Less(i, j int) bool { return a[i].heartbeat.Before(a[j].heartbeat) }
func (a addresssByHeartbeat) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }

// accountSet is simply a set of addresses to check for existance, and a signer
// capable of deriving addresses from transactions.
type accountSet struct {
	accounts map[common.Address]struct{}
	signer   types.Signer
}

// newAccountSet creates a new address set with an associated signer for sender
// derivations.
func newAccountSet(signer types.Signer) *accountSet {
	return &accountSet{
		accounts: make(map[common.Address]struct{}),
		signer:   signer,
	}
}

// contains checks if a given address is contained within the set.
func (as *accountSet) contains(addr common.Address) bool {
	_, exist := as.accounts[addr]
	return exist
}

// containsTx checks if the sender of a given tx is within the set. If the sender
// cannot be derived, this method returns false.
func (as *accountSet) containsTx(tx *types.Transaction) bool {
	if addr, err := types.Sender(as.signer, tx); err == nil {
		return as.contains(addr)
	}
	return false
}

// add inserts a new address into the set to track.
func (as *accountSet) add(addr common.Address) {
	as.accounts[addr] = struct{}{}
}
