package testutil

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"go.thebigfile.com/core/types"
	"go.thebigfile.com/coreutils/chain"
	"golang.org/x/net/context"
)

// ElementStateStore is a store that holds state elements in memory.
// It is primarily used for testing proof updaters and other components that
// need to access state elements without persisting them to disk.
type ElementStateStore struct {
	chain *chain.Manager

	mu                   sync.Mutex
	tip                  types.ChainIndex
	bigfileElements      map[types.BigfileOutputID]types.BigfileElement
	bigfundElements      map[types.BigfundOutputID]types.BigfundElement
	fileContractElements map[types.FileContractID]types.FileContractElement
	chainIndexElements   map[types.ChainIndex]types.ChainIndexElement
}

// Sync triggers a sync of the ElementStateStore with the chain manager.
// It should not normally be called directly, as it is automatically
// triggered by the chain manager when a reorg occurs.
func (es *ElementStateStore) Sync() error {
	es.mu.Lock()
	defer es.mu.Unlock()

	for {
		reverted, applied, err := es.chain.UpdatesSince(es.tip, 1000)
		if err != nil {
			return fmt.Errorf("failed to get updates since %q: %w", es.tip, err)
		} else if len(reverted) == 0 && len(applied) == 0 {
			return nil
		}

		for _, cru := range reverted {
			revertedIndex := types.ChainIndex{
				Height: cru.State.Index.Height + 1,
				ID:     cru.Block.ID(),
			}
			for _, bige := range cru.BigfileElementDiffs() {
				switch {
				case bige.Created && bige.Spent:
					continue
				case bige.Created:
					delete(es.bigfileElements, bige.BigfileElement.ID)
				case bige.Spent:
					es.bigfileElements[bige.BigfileElement.ID] = bige.BigfileElement
				}
			}
			for _, bfe := range cru.BigfundElementDiffs() {
				switch {
				case bfe.Created && bfe.Spent:
					continue
				case bfe.Created:
					delete(es.bigfundElements, bfe.BigfundElement.ID)
				case bfe.Spent:
					es.bigfundElements[bfe.BigfundElement.ID] = bfe.BigfundElement
				}
			}
			for _, fce := range cru.FileContractElementDiffs() {
				switch {
				case fce.Resolved:
					es.fileContractElements[fce.FileContractElement.ID] = fce.FileContractElement
				case fce.Revision != nil:
					es.fileContractElements[fce.FileContractElement.ID] = fce.FileContractElement // revert the revision
				case fce.Created:
					delete(es.fileContractElements, fce.FileContractElement.ID)
				}
			}
			delete(es.chainIndexElements, revertedIndex)
			for id, bige := range es.bigfileElements {
				cru.UpdateElementProof(&bige.StateElement)
				es.bigfileElements[id] = bige.Copy()
			}
			for id, bfe := range es.bigfundElements {
				cru.UpdateElementProof(&bfe.StateElement)
				es.bigfundElements[id] = bfe.Copy()
			}
			for id, fce := range es.fileContractElements {
				cru.UpdateElementProof(&fce.StateElement)
				es.fileContractElements[id] = fce.Copy()
			}
			for id, cie := range es.chainIndexElements {
				cru.UpdateElementProof(&cie.StateElement)
				es.chainIndexElements[id] = cie.Copy()
			}
			es.tip = cru.State.Index
		}
		for _, cau := range applied {
			for _, bige := range cau.BigfileElementDiffs() {
				switch {
				case bige.Created && bige.Spent:
					continue
				case bige.Created:
					es.bigfileElements[bige.BigfileElement.ID] = bige.BigfileElement
				case bige.Spent:
					delete(es.bigfileElements, bige.BigfileElement.ID)
				}
			}
			for _, bfe := range cau.BigfundElementDiffs() {
				switch {
				case bfe.Created && bfe.Spent:
					continue
				case bfe.Created:
					es.bigfundElements[bfe.BigfundElement.ID] = bfe.BigfundElement
				case bfe.Spent:
					delete(es.bigfundElements, bfe.BigfundElement.ID)
				}
			}
			for _, fce := range cau.FileContractElementDiffs() {
				switch {
				case fce.Resolved:
					delete(es.fileContractElements, fce.FileContractElement.ID)
				case fce.Revision != nil:
					es.fileContractElements[fce.FileContractElement.ID], _ = fce.RevisionElement() // nil already checked
				case fce.Created:
					es.fileContractElements[fce.FileContractElement.ID] = fce.FileContractElement
				}
			}
			es.chainIndexElements[cau.State.Index] = cau.ChainIndexElement()
			for id, bige := range es.bigfileElements {
				cau.UpdateElementProof(&bige.StateElement)
				es.bigfileElements[id] = bige.Copy()
			}
			for id, bfe := range es.bigfundElements {
				cau.UpdateElementProof(&bfe.StateElement)
				es.bigfundElements[id] = bfe.Copy()
			}
			for id, fce := range es.fileContractElements {
				cau.UpdateElementProof(&fce.StateElement)
				es.fileContractElements[id] = fce.Copy()
			}
			for id, cie := range es.chainIndexElements {
				cau.UpdateElementProof(&cie.StateElement)
				es.chainIndexElements[id] = cie.Copy()
			}
			es.tip = cau.State.Index
		}
	}
}

// Wait blocks until the ElementStateStore is synced with the chain manager
// or the context cancelled.
func (es *ElementStateStore) Wait(tb testing.TB) {
	tb.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tb.Cleanup(func() {
		cancel()
	})

	t := time.NewTicker(time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			tb.Fatal("store sync timed out")
		case <-t.C:
			es.mu.Lock()
			synced := es.tip == es.chain.Tip()
			es.mu.Unlock()
			if synced {
				return
			}
		}
	}
}

// BigfileElements returns a copy of all BigfileElements in the store.
func (es *ElementStateStore) BigfileElements() (types.ChainIndex, []types.BigfileElement) {
	es.mu.Lock()
	defer es.mu.Unlock()
	elements := make([]types.BigfileElement, 0, len(es.bigfileElements))
	for _, bige := range es.bigfileElements {
		bige.StateElement = bige.StateElement.Copy()
		elements = append(elements, bige)
	}
	return es.tip, elements
}

// BigfileElement returns the BigfileElement with the given ID.
func (es *ElementStateStore) BigfileElement(id types.BigfileOutputID) (types.ChainIndex, types.BigfileElement, bool) {
	es.mu.Lock()
	defer es.mu.Unlock()
	bige, ok := es.bigfileElements[id]
	bige.StateElement = bige.StateElement.Copy()
	return es.tip, bige, ok
}

// BigfundElement returns the BigfundElement with the given ID.
func (es *ElementStateStore) BigfundElement(id types.BigfundOutputID) (types.ChainIndex, types.BigfundElement, bool) {
	es.mu.Lock()
	defer es.mu.Unlock()
	bfe, ok := es.bigfundElements[id]
	bfe.StateElement = bfe.StateElement.Copy()
	return es.tip, bfe, ok
}

// FileContractElement returns the FileContractElement with the given ID.
func (es *ElementStateStore) FileContractElement(id types.FileContractID) (types.ChainIndex, types.FileContractElement, bool) {
	es.mu.Lock()
	defer es.mu.Unlock()
	fce, ok := es.fileContractElements[id]
	fce.StateElement = fce.StateElement.Copy()
	return es.tip, fce, ok
}

// ChainIndexElement returns the ChainIndexElement for the given chain index.
func (es *ElementStateStore) ChainIndexElement(index types.ChainIndex) (types.ChainIndexElement, bool) {
	es.mu.Lock()
	defer es.mu.Unlock()
	cie, ok := es.chainIndexElements[index]
	cie.StateElement = cie.StateElement.Copy()
	return cie, ok
}

// NewElementStateStore creates a new ElementStateStore
func NewElementStateStore(tb testing.TB, cm *chain.Manager) *ElementStateStore {
	store := &ElementStateStore{
		chain:                cm,
		bigfileElements:      make(map[types.BigfileOutputID]types.BigfileElement),
		bigfundElements:      make(map[types.BigfundOutputID]types.BigfundElement),
		fileContractElements: make(map[types.FileContractID]types.FileContractElement),
		chainIndexElements:   make(map[types.ChainIndex]types.ChainIndexElement),
	}

	reorgCh := make(chan struct{}, 1)
	cancel := cm.OnReorg(func(types.ChainIndex) {
		select {
		case reorgCh <- struct{}{}:
		default:
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	tb.Cleanup(func() {
		cancel()
	})

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-reorgCh:
			}

			if err := store.Sync(); err != nil {
				panic(err)
			}
		}
	}()
	return store
}
