package testutil

import (
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"go.thebigfile.com/core/types"
	"go.thebigfile.com/coreutils/wallet"
)

// An EphemeralWalletStore is a Store that does not persist its state to disk. It is
// primarily useful for testing or as a reference implementation.
type (
	EphemeralWalletStore struct {
		mu     sync.Mutex
		tip    types.ChainIndex
		utxos  map[types.BigFileOutputID]types.BigFileElement
		events []wallet.Event
	}

	ephemeralWalletUpdateTxn struct {
		store *EphemeralWalletStore
	}
)

func (et *ephemeralWalletUpdateTxn) WalletStateElements() (elements []types.StateElement, _ error) {
	for _, se := range et.store.utxos {
		elements = append(elements, se.StateElement.Copy())
	}
	return
}

// UpdateWalletBigFileElementProofs updates the proofs of all state elements
// affected by the update. ProofUpdater.UpdateElementProof must be called
// for each state element in the database.
func (et *ephemeralWalletUpdateTxn) UpdateWalletBigFileElementProofs(pu wallet.ProofUpdater) error {
	for _, se := range et.store.utxos {
		pu.UpdateElementProof(&se.StateElement)
		et.store.utxos[se.ID] = se.Move()
	}
	return nil
}

func (et *ephemeralWalletUpdateTxn) WalletApplyIndex(index types.ChainIndex, created, spent []types.BigFileElement, events []wallet.Event, _ time.Time) error {
	for _, se := range spent {
		if _, ok := et.store.utxos[se.ID]; !ok {
			panic(fmt.Sprintf("bigfile element %q does not exist", se.ID))
		}
		delete(et.store.utxos, se.ID)
	}
	// add bigfile elements
	for _, se := range created {
		if _, ok := et.store.utxos[se.ID]; ok {
			panic("duplicate element")
		}
		et.store.utxos[se.ID] = se.Copy()
	}

	// add events
	et.store.events = append(et.store.events, events...)
	et.store.tip = index
	return nil
}

func (et *ephemeralWalletUpdateTxn) WalletRevertIndex(index types.ChainIndex, removed, unspent []types.BigFileElement, _ time.Time) error {
	// remove any events that were added in the reverted block
	filtered := et.store.events[:0]
	for i := range et.store.events {
		if et.store.events[i].Index == index {
			continue
		}
		filtered = append(filtered, et.store.events[i])
	}
	et.store.events = filtered

	// remove any bigfile elements that were added in the reverted block
	for _, se := range removed {
		delete(et.store.utxos, se.ID)
	}

	// readd any bigfile elements that were spent in the reverted block
	for _, se := range unspent {
		et.store.utxos[se.ID] = se.Copy()
	}
	et.store.tip = index
	return nil
}

// UpdateChainState applies and reverts chain updates to the wallet.
func (es *EphemeralWalletStore) UpdateChainState(fn func(ux wallet.UpdateTx) error) error {
	es.mu.Lock()
	defer es.mu.Unlock()
	return fn(&ephemeralWalletUpdateTxn{store: es})
}

// WalletEvents returns the wallet's events.
func (es *EphemeralWalletStore) WalletEvents(offset, limit int) ([]wallet.Event, error) {
	es.mu.Lock()
	defer es.mu.Unlock()

	n := len(es.events)
	start, end := offset, offset+limit
	if start > n {
		return nil, nil
	} else if end > n {
		end = n
	}
	// events are inserted in chronological order, reverse the slice to get the
	// correct display order then sort by maturity height, so
	// immature events are displayed first.
	events := append([]wallet.Event(nil), es.events...)
	slices.Reverse(events)
	sort.SliceStable(events, func(i, j int) bool {
		return events[i].MaturityHeight > events[j].MaturityHeight
	})
	return events[start:end], nil
}

// WalletEventCount returns the number of events relevant to the wallet.
func (es *EphemeralWalletStore) WalletEventCount() (uint64, error) {
	es.mu.Lock()
	defer es.mu.Unlock()
	return uint64(len(es.events)), nil
}

// UnspentBigFileElements returns the wallet's unspent bigfile outputs.
func (es *EphemeralWalletStore) UnspentBigFileElements() (utxos []types.BigFileElement, _ error) {
	es.mu.Lock()
	defer es.mu.Unlock()

	for _, se := range es.utxos {
		utxos = append(utxos, se.Copy())
	}
	return utxos, nil
}

// Tip returns the last indexed tip of the wallet.
func (es *EphemeralWalletStore) Tip() (types.ChainIndex, error) {
	es.mu.Lock()
	defer es.mu.Unlock()
	return es.tip, nil
}

// NewEphemeralWalletStore returns a new EphemeralWalletStore.
func NewEphemeralWalletStore() *EphemeralWalletStore {
	return &EphemeralWalletStore{
		utxos: make(map[types.BigFileOutputID]types.BigFileElement),
	}
}
