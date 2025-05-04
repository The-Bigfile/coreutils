package wallet

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"go.thebigfile.com/core/consensus"
	"go.thebigfile.com/core/types"
	"go.uber.org/zap"
)

const (
	// bytesPerInput is the encoded size of a BigFileInput and corresponding
	// TransactionSignature, assuming standard UnlockConditions.
	bytesPerInput = 241

	// redistributeBatchSize is the number of outputs to redistribute per txn to
	// avoid creating a txn that is too large.
	redistributeBatchSize = 10
)

var (
	// ErrNotEnoughFunds is returned when there are not enough unspent outputs
	// to fund a transaction.
	ErrNotEnoughFunds = errors.New("not enough funds")
)

type (
	// Balance is the balance of a wallet.
	Balance struct {
		Spendable   types.Currency `json:"spendable"`
		Confirmed   types.Currency `json:"confirmed"`
		Unconfirmed types.Currency `json:"unconfirmed"`
		Immature    types.Currency `json:"immature"`
	}

	// A ChainManager manages the current state of the blockchain.
	ChainManager interface {
		TipState() consensus.State
		BestIndex(height uint64) (types.ChainIndex, bool)
		PoolTransactions() []types.Transaction
		V2PoolTransactions() []types.V2Transaction
		OnReorg(func(types.ChainIndex)) func()
	}

	// A SingleAddressStore stores the state of a single-address wallet.
	// Implementations are assumed to be thread safe.
	SingleAddressStore interface {
		// Tip returns the consensus change ID and block height of
		// the last wallet change.
		Tip() (types.ChainIndex, error)
		// UnspentBigFileElements returns a list of all unspent bigfile outputs
		// including immature outputs.
		UnspentBigFileElements() ([]types.BigFileElement, error)
		// WalletEvents returns a paginated list of transactions ordered by
		// maturity height, descending. If no more transactions are available,
		// (nil, nil) should be returned.
		WalletEvents(offset, limit int) ([]Event, error)
		// WalletEventCount returns the total number of events relevant to the
		// wallet.
		WalletEventCount() (uint64, error)
	}

	// A SingleAddressWallet is a hot wallet that manages the outputs controlled
	// by a single address.
	SingleAddressWallet struct {
		priv types.PrivateKey
		addr types.Address

		cm    ChainManager
		store SingleAddressStore
		log   *zap.Logger

		cfg config

		mu  sync.Mutex // protects the following fields
		tip types.ChainIndex
		// locked is a set of bigfile output IDs locked by FundTransaction. They
		// will be released either by calling Release for unused transactions or
		// being confirmed in a block.
		locked map[types.BigFileOutputID]time.Time
	}
)

// ErrDifferentSeed is returned when a different seed is provided to
// NewSingleAddressWallet than was used to initialize the wallet
var ErrDifferentSeed = errors.New("seed differs from wallet seed")

// Close closes the wallet
func (sw *SingleAddressWallet) Close() error {
	return nil
}

// Address returns the address of the wallet.
func (sw *SingleAddressWallet) Address() types.Address {
	return sw.addr
}

// UnlockConditions returns the unlock conditions of the wallet.
func (sw *SingleAddressWallet) UnlockConditions() types.UnlockConditions {
	return types.StandardUnlockConditions(sw.priv.PublicKey())
}

// UnspentBigFileElements returns the wallet's unspent bigfile outputs.
func (sw *SingleAddressWallet) UnspentBigFileElements() ([]types.BigFileElement, error) {
	return sw.store.UnspentBigFileElements()
}

// Balance returns the balance of the wallet.
func (sw *SingleAddressWallet) Balance() (balance Balance, err error) {
	outputs, err := sw.store.UnspentBigFileElements()
	if err != nil {
		return Balance{}, fmt.Errorf("failed to get unspent outputs: %w", err)
	}

	tpoolSpent := make(map[types.BigFileOutputID]bool)
	tpoolUtxos := make(map[types.BigFileOutputID]types.BigFileElement)
	for _, txn := range sw.cm.PoolTransactions() {
		for _, bigi := range txn.BigFileInputs {
			if bigi.UnlockConditions.UnlockHash() != sw.addr {
				continue
			}
			tpoolSpent[bigi.ParentID] = true
			delete(tpoolUtxos, bigi.ParentID)
		}
		for i, bigo := range txn.BigFileOutputs {
			if bigo.Address != sw.addr {
				continue
			}

			outputID := txn.BigFileOutputID(i)
			tpoolUtxos[outputID] = types.BigFileElement{
				ID:            types.BigFileOutputID(outputID),
				StateElement:  types.StateElement{LeafIndex: types.UnassignedLeafIndex},
				BigFileOutput: bigo,
			}
		}
	}

	for _, txn := range sw.cm.V2PoolTransactions() {
		for _, si := range txn.BigFileInputs {
			if si.Parent.BigFileOutput.Address != sw.addr {
				continue
			}
			tpoolSpent[si.Parent.ID] = true
			delete(tpoolUtxos, si.Parent.ID)
		}
		for i, bigo := range txn.BigFileOutputs {
			if bigo.Address != sw.addr {
				continue
			}
			bige := txn.EphemeralBigFileOutput(i)
			tpoolUtxos[bige.ID] = bige.Move()
		}
	}

	sw.mu.Lock()
	defer sw.mu.Unlock()
	bh := sw.cm.TipState().Index.Height
	for _, bigo := range outputs {
		if bigo.MaturityHeight > bh {
			balance.Immature = balance.Immature.Add(bigo.BigFileOutput.Value)
		} else {
			balance.Confirmed = balance.Confirmed.Add(bigo.BigFileOutput.Value)
			if !sw.isLocked(bigo.ID) && !tpoolSpent[bigo.ID] {
				balance.Spendable = balance.Spendable.Add(bigo.BigFileOutput.Value)
			}
		}
	}

	for _, bigo := range tpoolUtxos {
		balance.Unconfirmed = balance.Unconfirmed.Add(bigo.BigFileOutput.Value)
	}
	return
}

// Events returns a paginated list of events, ordered by maturity height, descending.
// If no more events are available, (nil, nil) is returned.
func (sw *SingleAddressWallet) Events(offset, limit int) ([]Event, error) {
	return sw.store.WalletEvents(offset, limit)
}

// EventCount returns the total number of events relevant to the wallet.
func (sw *SingleAddressWallet) EventCount() (uint64, error) {
	return sw.store.WalletEventCount()
}

// SpendableOutputs returns a list of spendable bigfile outputs, a spendable
// output is an unspent output that's not locked, not currently in the
// transaction pool and that has matured.
func (sw *SingleAddressWallet) SpendableOutputs() ([]types.BigFileElement, error) {
	// fetch outputs from the store
	utxos, err := sw.store.UnspentBigFileElements()
	if err != nil {
		return nil, err
	}

	// fetch outputs currently in the pool
	inPool := make(map[types.BigFileOutputID]bool)
	for _, txn := range sw.cm.PoolTransactions() {
		for _, bigi := range txn.BigFileInputs {
			inPool[bigi.ParentID] = true
		}
	}

	// grab current height
	state := sw.cm.TipState()
	bh := state.Index.Height

	sw.mu.Lock()
	defer sw.mu.Unlock()

	// filter outputs that are either locked, in the pool or have not yet matured
	unspent := utxos[:0]
	for _, bige := range utxos {
		if sw.isLocked(bige.ID) || inPool[bige.ID] || bh < bige.MaturityHeight {
			continue
		}
		unspent = append(unspent, bige.Copy())
	}
	return unspent, nil
}

func (sw *SingleAddressWallet) selectUTXOs(amount types.Currency, inputs int, useUnconfirmed bool, elements []types.BigFileElement) ([]types.BigFileElement, types.Currency, error) {
	if amount.IsZero() {
		return nil, types.ZeroCurrency, nil
	}

	tpoolSpent := make(map[types.BigFileOutputID]bool)
	tpoolUtxos := make(map[types.BigFileOutputID]types.BigFileElement)
	for _, txn := range sw.cm.PoolTransactions() {
		for _, bigi := range txn.BigFileInputs {
			tpoolSpent[bigi.ParentID] = true
			delete(tpoolUtxos, bigi.ParentID)
		}
		for i, bigo := range txn.BigFileOutputs {
			tpoolUtxos[txn.BigFileOutputID(i)] = types.BigFileElement{
				ID:            txn.BigFileOutputID(i),
				StateElement:  types.StateElement{LeafIndex: types.UnassignedLeafIndex},
				BigFileOutput: bigo,
			}
		}
	}
	for _, txn := range sw.cm.V2PoolTransactions() {
		for _, bigi := range txn.BigFileInputs {
			tpoolSpent[bigi.Parent.ID] = true
			delete(tpoolUtxos, bigi.Parent.ID)
		}
		for i := range txn.BigFileOutputs {
			bige := txn.EphemeralBigFileOutput(i)
			tpoolUtxos[bige.ID] = bige.Move()
		}
	}

	// remove immature, locked and spent outputs
	cs := sw.cm.TipState()
	utxos := make([]types.BigFileElement, 0, len(elements))
	var usedSum types.Currency
	var immatureSum types.Currency
	for _, bige := range elements {
		if used := sw.isLocked(bige.ID) || tpoolSpent[bige.ID]; used {
			usedSum = usedSum.Add(bige.BigFileOutput.Value)
			continue
		} else if immature := cs.Index.Height < bige.MaturityHeight; immature {
			immatureSum = immatureSum.Add(bige.BigFileOutput.Value)
			continue
		}
		utxos = append(utxos, bige.Share())
	}

	// sort by value, descending
	sort.Slice(utxos, func(i, j int) bool {
		return utxos[i].BigFileOutput.Value.Cmp(utxos[j].BigFileOutput.Value) > 0
	})

	var unconfirmedUTXOs []types.BigFileElement
	var unconfirmedSum types.Currency
	if useUnconfirmed {
		for _, bige := range tpoolUtxos {
			if bige.BigFileOutput.Address != sw.addr || sw.isLocked(bige.ID) {
				continue
			}
			unconfirmedUTXOs = append(unconfirmedUTXOs, bige.Share())
			unconfirmedSum = unconfirmedSum.Add(bige.BigFileOutput.Value)
		}
	}

	// sort by value, descending
	sort.Slice(unconfirmedUTXOs, func(i, j int) bool {
		return unconfirmedUTXOs[i].BigFileOutput.Value.Cmp(unconfirmedUTXOs[j].BigFileOutput.Value) > 0
	})

	// fund the transaction using the largest utxos first
	var selected []types.BigFileElement
	var inputSum types.Currency
	for i, bige := range utxos {
		if inputSum.Cmp(amount) >= 0 {
			utxos = utxos[i:]
			break
		}
		selected = append(selected, bige.Share())
		inputSum = inputSum.Add(bige.BigFileOutput.Value)
	}

	if inputSum.Cmp(amount) < 0 && useUnconfirmed {
		// try adding unconfirmed utxos.
		for _, bige := range unconfirmedUTXOs {
			selected = append(selected, bige.Share())
			inputSum = inputSum.Add(bige.BigFileOutput.Value)
			if inputSum.Cmp(amount) >= 0 {
				break
			}
		}

		if inputSum.Cmp(amount) < 0 {
			// still not enough funds
			return nil, types.ZeroCurrency, fmt.Errorf("%w: inputs %v < needed %v (used: %v immature: %v unconfirmed: %v)", ErrNotEnoughFunds, inputSum.String(), amount.String(), usedSum.String(), immatureSum.String(), unconfirmedSum.String())
		}
	} else if inputSum.Cmp(amount) < 0 {
		return nil, types.ZeroCurrency, fmt.Errorf("%w: inputs %v < needed %v (used: %v immature: %v", ErrNotEnoughFunds, inputSum.String(), amount.String(), usedSum.String(), immatureSum.String())
	}

	// check if remaining utxos should be defragged
	txnInputs := inputs + len(selected)
	if len(utxos) > sw.cfg.DefragThreshold && txnInputs < sw.cfg.MaxInputsForDefrag {
		// add the smallest utxos to the transaction
		defraggable := utxos
		if len(defraggable) > sw.cfg.MaxDefragUTXOs {
			defraggable = defraggable[len(defraggable)-sw.cfg.MaxDefragUTXOs:]
		}
		for i := len(defraggable) - 1; i >= 0; i-- {
			if txnInputs >= sw.cfg.MaxInputsForDefrag {
				break
			}

			bige := &defraggable[i]
			selected = append(selected, bige.Share())
			inputSum = inputSum.Add(bige.BigFileOutput.Value)
			txnInputs++
		}
	}
	return selected, inputSum, nil
}

// FundTransaction adds bigfile inputs worth at least amount to the provided
// transaction. If necessary, a change output will also be added. The inputs
// will not be available to future calls to FundTransaction unless ReleaseInputs
// is called.
func (sw *SingleAddressWallet) FundTransaction(txn *types.Transaction, amount types.Currency, useUnconfirmed bool) ([]types.Hash256, error) {
	if amount.IsZero() {
		return nil, nil
	}

	elements, err := sw.store.UnspentBigFileElements()
	if err != nil {
		return nil, err
	}

	sw.mu.Lock()
	defer sw.mu.Unlock()

	selected, inputSum, err := sw.selectUTXOs(amount, len(txn.BigFileInputs), useUnconfirmed, elements)
	if err != nil {
		return nil, err
	}

	// add a change output if necessary
	if inputSum.Cmp(amount) > 0 {
		txn.BigFileOutputs = append(txn.BigFileOutputs, types.BigFileOutput{
			Value:   inputSum.Sub(amount),
			Address: sw.addr,
		})
	}

	toSign := make([]types.Hash256, len(selected))
	for i, bige := range selected {
		txn.BigFileInputs = append(txn.BigFileInputs, types.BigFileInput{
			ParentID:         bige.ID,
			UnlockConditions: types.StandardUnlockConditions(sw.priv.PublicKey()),
		})
		toSign[i] = types.Hash256(bige.ID)
		sw.locked[bige.ID] = time.Now().Add(sw.cfg.ReservationDuration)
	}

	return toSign, nil
}

// SignTransaction adds a signature to each of the specified inputs.
func (sw *SingleAddressWallet) SignTransaction(txn *types.Transaction, toSign []types.Hash256, cf types.CoveredFields) {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	state := sw.cm.TipState()

	for _, id := range toSign {
		var h types.Hash256
		if cf.WholeTransaction {
			h = state.WholeSigHash(*txn, id, 0, 0, cf.Signatures)
		} else {
			h = state.PartialSigHash(*txn, cf)
		}
		sig := sw.priv.SignHash(h)
		txn.Signatures = append(txn.Signatures, types.TransactionSignature{
			ParentID:       id,
			CoveredFields:  cf,
			PublicKeyIndex: 0,
			Signature:      sig[:],
		})
	}
}

// FundV2Transaction adds bigfile inputs worth at least amount to the provided
// transaction. If necessary, a change output will also be added. The inputs
// will not be available to future calls to FundTransaction unless ReleaseInputs
// is called.
//
// The returned index should be used as the basis for AddV2PoolTransactions.
func (sw *SingleAddressWallet) FundV2Transaction(txn *types.V2Transaction, amount types.Currency, useUnconfirmed bool) (types.ChainIndex, []int, error) {
	if amount.IsZero() {
		return sw.tip, nil, nil
	}

	// fetch outputs from the store
	elements, err := sw.store.UnspentBigFileElements()
	if err != nil {
		return types.ChainIndex{}, nil, err
	}

	sw.mu.Lock()
	defer sw.mu.Unlock()

	selected, inputSum, err := sw.selectUTXOs(amount, len(txn.BigFileInputs), useUnconfirmed, elements)
	if err != nil {
		return types.ChainIndex{}, nil, err
	}

	// add a change output if necessary
	if inputSum.Cmp(amount) > 0 {
		txn.BigFileOutputs = append(txn.BigFileOutputs, types.BigFileOutput{
			Value:   inputSum.Sub(amount),
			Address: sw.addr,
		})
	}

	toSign := make([]int, 0, len(selected))
	for _, bige := range selected {
		toSign = append(toSign, len(txn.BigFileInputs))
		txn.BigFileInputs = append(txn.BigFileInputs, types.V2BigFileInput{
			Parent: bige.Copy(),
		})
		sw.locked[bige.ID] = time.Now().Add(sw.cfg.ReservationDuration)
	}

	return sw.tip, toSign, nil
}

// SignV2Inputs adds a signature to each of the specified bigfile inputs.
func (sw *SingleAddressWallet) SignV2Inputs(txn *types.V2Transaction, toSign []int) {
	if len(toSign) == 0 {
		return
	}

	sw.mu.Lock()
	defer sw.mu.Unlock()

	policy := sw.SpendPolicy()
	sigHash := sw.cm.TipState().InputSigHash(*txn)
	for _, i := range toSign {
		txn.BigFileInputs[i].SatisfiedPolicy = types.SatisfiedPolicy{
			Policy:     policy,
			Signatures: []types.Signature{sw.SignHash(sigHash)},
		}
	}
}

// Tip returns the block height the wallet has scanned to.
func (sw *SingleAddressWallet) Tip() types.ChainIndex {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.tip
}

// SpendPolicy returns the wallet's default spend policy.
func (sw *SingleAddressWallet) SpendPolicy() types.SpendPolicy {
	return types.SpendPolicy{Type: types.PolicyTypeUnlockConditions(sw.UnlockConditions())}
}

// SignHash signs the hash with the wallet's private key.
func (sw *SingleAddressWallet) SignHash(h types.Hash256) types.Signature {
	return sw.priv.SignHash(h)
}

// UnconfirmedEvents returns all unconfirmed transactions relevant to the
// wallet.
func (sw *SingleAddressWallet) UnconfirmedEvents() (annotated []Event, err error) {
	confirmed, err := sw.store.UnspentBigFileElements()
	if err != nil {
		return nil, fmt.Errorf("failed to get unspent outputs: %w", err)
	}

	utxos := make(map[types.BigFileOutputID]types.BigFileElement)
	for _, se := range confirmed {
		utxos[se.ID] = se.Share()
	}

	index := types.ChainIndex{
		Height: sw.cm.TipState().Index.Height + 1,
	}
	timestamp := time.Now().Truncate(time.Second)

	addEvent := func(id types.Hash256, eventType string, data EventData) {
		ev := Event{
			ID:             id,
			Index:          index,
			MaturityHeight: index.Height,
			Timestamp:      timestamp,
			Type:           eventType,
			Data:           data,
			Relevant:       []types.Address{sw.addr},
		}

		if ev.BigFileInflow().Equals(ev.BigFileOutflow()) {
			// ignore events that don't affect the wallet
			return
		}
		annotated = append(annotated, ev)
	}

	for _, txn := range sw.cm.PoolTransactions() {
		event := EventV1Transaction{
			Transaction: txn,
		}

		var outflow types.Currency
		for _, bigi := range txn.BigFileInputs {
			bige, ok := utxos[bigi.ParentID]
			if !ok {
				// ignore inputs that don't belong to the wallet
				continue
			}
			outflow = outflow.Add(bige.BigFileOutput.Value)
			event.SpentBigFileElements = append(event.SpentBigFileElements, bige.Share())
		}

		var inflow types.Currency
		for i, so := range txn.BigFileOutputs {
			if so.Address == sw.addr {
				inflow = inflow.Add(so.Value)
				utxos[txn.BigFileOutputID(i)] = types.BigFileElement{
					ID:            txn.BigFileOutputID(i),
					StateElement:  types.StateElement{LeafIndex: types.UnassignedLeafIndex},
					BigFileOutput: so,
				}
			}
		}

		// skip transactions that don't affect the wallet
		if inflow.IsZero() && outflow.IsZero() {
			continue
		}
		addEvent(types.Hash256(txn.ID()), EventTypeV1Transaction, event)
	}

	for _, txn := range sw.cm.V2PoolTransactions() {
		var inflow, outflow types.Currency
		for _, bigi := range txn.BigFileInputs {
			if bigi.Parent.BigFileOutput.Address != sw.addr {
				continue
			}
			outflow = outflow.Add(bigi.Parent.BigFileOutput.Value)
		}

		for _, bigo := range txn.BigFileOutputs {
			if bigo.Address != sw.addr {
				continue
			}
			inflow = inflow.Add(bigo.Value)
		}

		// skip transactions that don't affect the wallet
		if inflow.IsZero() && outflow.IsZero() {
			continue
		}

		addEvent(types.Hash256(txn.ID()), EventTypeV2Transaction, EventV2Transaction(txn))
	}
	return annotated, nil
}

func (sw *SingleAddressWallet) selectRedistributeUTXOs(bh uint64, outputs int, amount types.Currency, elements []types.BigFileElement) ([]types.BigFileElement, int, error) {
	// fetch outputs currently in the pool
	inPool := make(map[types.BigFileOutputID]bool)
	for _, txn := range sw.cm.PoolTransactions() {
		for _, bigi := range txn.BigFileInputs {
			inPool[bigi.ParentID] = true
		}
	}
	for _, txn := range sw.cm.V2PoolTransactions() {
		for _, bigi := range txn.BigFileInputs {
			inPool[bigi.Parent.ID] = true
		}
	}

	// adjust the number of desired outputs for any output we encounter that is
	// unused, matured and has the same value
	utxos := make([]types.BigFileElement, 0, len(elements))
	for _, bige := range elements {
		inUse := sw.isLocked(bige.ID) || inPool[bige.ID]
		matured := bh >= bige.MaturityHeight
		sameValue := bige.BigFileOutput.Value.Equals(amount)

		// adjust number of desired outputs
		if !inUse && matured && sameValue {
			outputs--
		}

		// collect usable outputs for defragging
		if !inUse && matured && !sameValue {
			utxos = append(utxos, bige.Share())
		}
	}
	// desc sort
	sort.Slice(utxos, func(i, j int) bool {
		return utxos[i].BigFileOutput.Value.Cmp(utxos[j].BigFileOutput.Value) > 0
	})
	return utxos, outputs, nil
}

// Redistribute returns a transaction that redistributes money in the wallet by
// selecting a minimal set of inputs to cover the creation of the requested
// outputs. It also returns a list of output IDs that need to be signed.
func (sw *SingleAddressWallet) Redistribute(outputs int, amount, feePerByte types.Currency) (txns []types.Transaction, toSign [][]types.Hash256, err error) {
	state := sw.cm.TipState()

	elements, err := sw.store.UnspentBigFileElements()
	if err != nil {
		return nil, nil, err
	}

	sw.mu.Lock()
	defer sw.mu.Unlock()

	utxos, outputs, err := sw.selectRedistributeUTXOs(state.Index.Height, outputs, amount, elements)
	if err != nil {
		return nil, nil, err
	}

	// return early if we don't have to defrag at all
	if outputs <= 0 {
		return nil, nil, nil
	}

	// in case of an error we need to free all inputs
	defer func() {
		if err != nil {
			for _, ids := range toSign {
				for _, id := range ids {
					delete(sw.locked, types.BigFileOutputID(id))
				}
			}
		}
	}()

	// desc sort
	sort.Slice(utxos, func(i, j int) bool {
		return utxos[i].BigFileOutput.Value.Cmp(utxos[j].BigFileOutput.Value) > 0
	})

	// prepare defrag transactions
	for outputs > 0 {
		var txn types.Transaction
		for i := 0; i < outputs && i < redistributeBatchSize; i++ {
			txn.BigFileOutputs = append(txn.BigFileOutputs, types.BigFileOutput{
				Value:   amount,
				Address: sw.addr,
			})
		}
		outputs -= len(txn.BigFileOutputs)

		// estimate the fees
		outputFees := feePerByte.Mul64(state.TransactionWeight(txn))
		feePerInput := feePerByte.Mul64(bytesPerInput)

		// collect outputs that cover the total amount
		var inputs []types.BigFileElement
		want := amount.Mul64(uint64(len(txn.BigFileOutputs)))
		for _, bige := range utxos {
			inputs = append(inputs, bige.Share())
			fee := feePerInput.Mul64(uint64(len(inputs))).Add(outputFees)
			if SumOutputs(inputs).Cmp(want.Add(fee)) > 0 {
				break
			}
		}

		// remove used inputs from utxos
		utxos = utxos[len(inputs):]

		// not enough outputs found
		fee := feePerInput.Mul64(uint64(len(inputs))).Add(outputFees)
		if sumOut := SumOutputs(inputs); sumOut.Cmp(want.Add(fee)) < 0 {
			if len(txns) > 0 {
				// consider redistributing successful if we could generate at least one txn
				break
			}
			return nil, nil, fmt.Errorf("%w: inputs %v < needed %v + txnFee %v", ErrNotEnoughFunds, sumOut.String(), want.String(), fee.String())
		}

		// set the miner fee
		if !fee.IsZero() {
			txn.MinerFees = []types.Currency{fee}
		}

		// add the change output
		change := SumOutputs(inputs).Sub(want.Add(fee))
		if !change.IsZero() {
			txn.BigFileOutputs = append(txn.BigFileOutputs, types.BigFileOutput{
				Value:   change,
				Address: sw.addr,
			})
		}

		// add the inputs
		toSignTxn := make([]types.Hash256, 0, len(inputs))
		for _, bige := range inputs {
			toSignTxn = append(toSignTxn, types.Hash256(bige.ID))
			txn.BigFileInputs = append(txn.BigFileInputs, types.BigFileInput{
				ParentID:         bige.ID,
				UnlockConditions: types.StandardUnlockConditions(sw.priv.PublicKey()),
			})
			sw.locked[bige.ID] = time.Now().Add(sw.cfg.ReservationDuration)
		}
		txns = append(txns, txn)
		toSign = append(toSign, toSignTxn)
	}

	return
}

// RedistributeV2 returns a transaction that redistributes money in the wallet
// by selecting a minimal set of inputs to cover the creation of the requested
// outputs. It also returns a list of output IDs that need to be signed.
func (sw *SingleAddressWallet) RedistributeV2(outputs int, amount, feePerByte types.Currency) (txns []types.V2Transaction, toSign [][]int, err error) {
	state := sw.cm.TipState()

	elements, err := sw.store.UnspentBigFileElements()
	if err != nil {
		return nil, nil, err
	}

	sw.mu.Lock()
	defer sw.mu.Unlock()

	utxos, outputs, err := sw.selectRedistributeUTXOs(state.Index.Height, outputs, amount, elements)
	if err != nil {
		return nil, nil, err
	}

	// return early if we don't have to defrag at all
	if outputs <= 0 {
		return nil, nil, nil
	}

	// in case of an error we need to free all inputs
	defer func() {
		if err != nil {
			for txnIdx, toSignTxn := range toSign {
				for i := range toSignTxn {
					delete(sw.locked, txns[txnIdx].BigFileInputs[i].Parent.ID)
				}
			}
		}
	}()

	// prepare defrag transactions
	for outputs > 0 {
		var txn types.V2Transaction
		for i := 0; i < outputs && i < redistributeBatchSize; i++ {
			txn.BigFileOutputs = append(txn.BigFileOutputs, types.BigFileOutput{
				Value:   amount,
				Address: sw.addr,
			})
		}
		outputs -= len(txn.BigFileOutputs)

		// estimate the fees
		outputFees := feePerByte.Mul64(state.V2TransactionWeight(txn))
		feePerInput := feePerByte.Mul64(bytesPerInput)

		// collect outputs that cover the total amount
		var inputs []types.BigFileElement
		want := amount.Mul64(uint64(len(txn.BigFileOutputs)))
		for _, bige := range utxos {
			inputs = append(inputs, bige.Copy())
			fee := feePerInput.Mul64(uint64(len(inputs))).Add(outputFees)
			if SumOutputs(inputs).Cmp(want.Add(fee)) > 0 {
				break
			}
		}

		// remove used inputs from utxos
		utxos = utxos[len(inputs):]

		// not enough outputs found
		fee := feePerInput.Mul64(uint64(len(inputs))).Add(outputFees)
		if sumOut := SumOutputs(inputs); sumOut.Cmp(want.Add(fee)) < 0 {
			if len(txns) > 0 {
				// consider redistributing successful if we could generate at least one txn
				break
			}
			return nil, nil, fmt.Errorf("%w: inputs %v < needed %v + txnFee %v", ErrNotEnoughFunds, sumOut.String(), want.String(), fee.String())
		}

		// set the miner fee
		if !fee.IsZero() {
			txn.MinerFee = fee
		}

		// add the change output
		change := SumOutputs(inputs).Sub(want.Add(fee))
		if !change.IsZero() {
			txn.BigFileOutputs = append(txn.BigFileOutputs, types.BigFileOutput{
				Value:   change,
				Address: sw.addr,
			})
		}

		// add the inputs
		toSignTxn := make([]int, 0, len(inputs))
		for _, bige := range inputs {
			toSignTxn = append(toSignTxn, len(txn.BigFileInputs))
			txn.BigFileInputs = append(txn.BigFileInputs, types.V2BigFileInput{
				Parent: bige.Move(),
			})
			sw.locked[bige.ID] = time.Now().Add(sw.cfg.ReservationDuration)
		}
		txns = append(txns, txn)
		toSign = append(toSign, toSignTxn)
	}
	return
}

// ReleaseInputs is a helper function that releases the inputs of txn for use in
// other transactions. It should only be called on transactions that are invalid
// or will never be broadcast.
func (sw *SingleAddressWallet) ReleaseInputs(txns []types.Transaction, v2txns []types.V2Transaction) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	for _, txn := range txns {
		for _, in := range txn.BigFileInputs {
			delete(sw.locked, in.ParentID)
		}
	}
	for _, txn := range v2txns {
		for _, in := range txn.BigFileInputs {
			delete(sw.locked, in.Parent.ID)
		}
	}
}

// isLocked returns true if the bigfile output with given id is locked, this
// method must be called whilst holding the mutex lock.
func (sw *SingleAddressWallet) isLocked(id types.BigFileOutputID) bool {
	return time.Now().Before(sw.locked[id])
}

// IsRelevantTransaction returns true if the v1 transaction is relevant to the
// address
func IsRelevantTransaction(txn types.Transaction, addr types.Address) bool {
	for _, bigi := range txn.BigFileInputs {
		if bigi.UnlockConditions.UnlockHash() == addr {
			return true
		}
	}

	for _, bigo := range txn.BigFileOutputs {
		if bigo.Address == addr {
			return true
		}
	}

	for _, bigi := range txn.BigfundInputs {
		if bigi.UnlockConditions.UnlockHash() == addr {
			return true
		}
	}

	for _, bfo := range txn.BigfundOutputs {
		if bfo.Address == addr {
			return true
		}
	}
	return false
}

// ExplicitCoveredFields returns a CoveredFields that covers all elements
// present in txn.
func ExplicitCoveredFields(txn types.Transaction) (cf types.CoveredFields) {
	for i := range txn.BigFileInputs {
		cf.BigFileInputs = append(cf.BigFileInputs, uint64(i))
	}
	for i := range txn.BigFileOutputs {
		cf.BigFileOutputs = append(cf.BigFileOutputs, uint64(i))
	}
	for i := range txn.FileContracts {
		cf.FileContracts = append(cf.FileContracts, uint64(i))
	}
	for i := range txn.FileContractRevisions {
		cf.FileContractRevisions = append(cf.FileContractRevisions, uint64(i))
	}
	for i := range txn.StorageProofs {
		cf.StorageProofs = append(cf.StorageProofs, uint64(i))
	}
	for i := range txn.BigfundInputs {
		cf.BigfundInputs = append(cf.BigfundInputs, uint64(i))
	}
	for i := range txn.BigfundOutputs {
		cf.BigfundOutputs = append(cf.BigfundOutputs, uint64(i))
	}
	for i := range txn.MinerFees {
		cf.MinerFees = append(cf.MinerFees, uint64(i))
	}
	for i := range txn.ArbitraryData {
		cf.ArbitraryData = append(cf.ArbitraryData, uint64(i))
	}
	for i := range txn.Signatures {
		cf.Signatures = append(cf.Signatures, uint64(i))
	}
	return
}

// SumOutputs returns the total value of the supplied outputs.
func SumOutputs(outputs []types.BigFileElement) (sum types.Currency) {
	for _, o := range outputs {
		sum = sum.Add(o.BigFileOutput.Value)
	}
	return
}

// NewSingleAddressWallet returns a new SingleAddressWallet using the provided
// private key and store.
func NewSingleAddressWallet(priv types.PrivateKey, cm ChainManager, store SingleAddressStore, opts ...Option) (*SingleAddressWallet, error) {
	cfg := config{
		DefragThreshold:     30,
		MaxInputsForDefrag:  30,
		MaxDefragUTXOs:      10,
		ReservationDuration: 3 * time.Hour,
		Log:                 zap.NewNop(),
	}

	for _, opt := range opts {
		opt(&cfg)
	}

	tip, err := store.Tip()
	if err != nil {
		return nil, fmt.Errorf("failed to get wallet tip: %w", err)
	}

	sw := &SingleAddressWallet{
		priv: priv,

		store: store,
		cm:    cm,

		cfg: cfg,
		log: cfg.Log,

		addr:   types.StandardUnlockHash(priv.PublicKey()),
		tip:    tip,
		locked: make(map[types.BigFileOutputID]time.Time),
	}
	return sw, nil
}
