package chain_test

import (
	"testing"
	"time"

	"go.thebigfile.com/core/types"
	"go.thebigfile.com/coreutils"
	"go.thebigfile.com/coreutils/chain"
	"go.thebigfile.com/coreutils/testutil"
	"lukechampine.com/frand"
)

type memState struct {
	index              types.ChainIndex
	utxos              map[types.BigFileOutputID]types.BigFileElement
	chainIndexElements []types.ChainIndexElement
}

// Sync updates the memState to match the current state of the chain manager.
func (ms *memState) Sync(t *testing.T, cm *chain.Manager) {
	for cm.Tip() != ms.index {
		reverted, applied, err := cm.UpdatesSince(ms.index, 100)
		if err != nil {
			t.Fatal(err)
		}

		for _, cru := range reverted {
			revertedIndex := types.ChainIndex{
				ID:     cru.Block.ID(),
				Height: cru.State.Index.Height + 1,
			}

			// remove chain index element
			if len(ms.chainIndexElements) > 0 {
				last := ms.chainIndexElements[len(ms.chainIndexElements)-1].Copy()
				if last.ChainIndex != revertedIndex {
					t.Fatalf("expected reverted index %v, got %v", last.ChainIndex, revertedIndex)
				}
				ms.chainIndexElements = ms.chainIndexElements[:len(ms.chainIndexElements)-1]
			}

			// revert utxos
			for _, biged := range cru.BigFileElementDiffs() {
				bige := &biged.BigFileElement
				if bige.BigFileOutput.Address == types.AnyoneCanSpend().Address() {
					if biged.Spent {
						ms.utxos[bige.ID] = bige.Copy()
					}
					if biged.Created {
						delete(ms.utxos, bige.ID)
					}
				}
			}

			// update utxos proofs
			for key, se := range ms.utxos {
				cru.UpdateElementProof(&se.StateElement)
				ms.utxos[key] = se.Copy()
			}
			ms.index = cru.State.Index
		}

		for _, cau := range applied {
			// update chain index elements
			for i := range ms.chainIndexElements {
				cau.UpdateElementProof(&ms.chainIndexElements[i].StateElement)
			}
			// append new chain index element
			ms.chainIndexElements = append(ms.chainIndexElements, cau.ChainIndexElement())

			// apply utxos
			for _, biged := range cau.BigFileElementDiffs() {
				bige := &biged.BigFileElement
				if bige.BigFileOutput.Address == types.AnyoneCanSpend().Address() {
					if biged.Created {
						ms.utxos[bige.ID] = bige.Copy()
					}
					if biged.Spent {
						delete(ms.utxos, bige.ID)
					}
				}
			}

			// update utxos proofs
			for key, se := range ms.utxos {
				cau.UpdateElementProof(&se.StateElement)
				ms.utxos[key] = se.Move()
			}
			ms.index = cau.State.Index
		}
	}
}

// SpendableElement returns the first spendable BigFile utxo.
func (ms *memState) SpendableElement(t *testing.T) (se types.BigFileElement) {
	for _, se = range ms.utxos {
		if se.MaturityHeight <= ms.index.Height {
			return
		}
	}
	t.Fatal("no spendable utxos")
	return
}

func newMemState() *memState {
	return &memState{
		utxos: make(map[types.BigFileOutputID]types.BigFileElement),
	}
}

func TestV2Attestations(t *testing.T) {
	n, genesisBlock := testutil.V2Network()

	policy := types.AnyoneCanSpend()
	addr := policy.Address()

	mineBlocks := func(t *testing.T, cm *chain.Manager, n int) {
		t.Helper()

		for i := 0; i < n; i++ {
			b, ok := coreutils.MineBlock(cm, addr, 5*time.Second)
			if !ok {
				t.Fatal("failed to mine block")
			} else if err := cm.AddBlocks([]types.Block{b}); err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Run("arbitrary data", func(t *testing.T) {
		store, tipState, err := chain.NewDBStore(chain.NewMemDB(), n, genesisBlock, nil)
		if err != nil {
			t.Fatal(err)
		}
		cm := chain.NewManager(store, tipState)
		ms := newMemState()

		// mine until a utxo is spendable
		mineBlocks(t, cm, int(n.MaturityDelay)+1)
		ms.Sync(t, cm)

		txn := types.V2Transaction{
			ArbitraryData: frand.Bytes(16),
		}

		if _, err := cm.AddV2PoolTransactions(cm.Tip(), []types.V2Transaction{txn}); err != nil {
			t.Fatal(err)
		}

		mineBlocks(t, cm, 1)
		ms.Sync(t, cm)

		txn2 := types.V2Transaction{
			ArbitraryData: frand.Bytes(16),
		}

		if _, err := cm.AddV2PoolTransactions(cm.Tip(), []types.V2Transaction{txn2}); err != nil {
			t.Fatal(err)
		}

		mineBlocks(t, cm, 1)
		ms.Sync(t, cm)
	})

	t.Run("arbitrary data + attestation + no change output", func(t *testing.T) {
		store, tipState, err := chain.NewDBStore(chain.NewMemDB(), n, genesisBlock, nil)
		if err != nil {
			t.Fatal(err)
		}
		cm := chain.NewManager(store, tipState)
		ms := newMemState()

		// mine until a utxo is spendable
		mineBlocks(t, cm, int(n.MaturityDelay)+1)
		ms.Sync(t, cm)

		sk := types.GeneratePrivateKey()
		ann := chain.V2HostAnnouncement{
			{Address: "foo.bar:1234", Protocol: "tcp"},
		}
		se := ms.SpendableElement(t)
		txn := types.V2Transaction{
			BigFileInputs: []types.V2BigFileInput{
				{Parent: se.Copy(), SatisfiedPolicy: types.SatisfiedPolicy{Policy: policy}},
			},
			MinerFee:      se.BigFileOutput.Value,
			ArbitraryData: frand.Bytes(16),
			Attestations: []types.Attestation{
				ann.ToAttestation(cm.TipState(), sk),
			},
		}

		if _, err := cm.AddV2PoolTransactions(cm.Tip(), []types.V2Transaction{txn}); err != nil {
			t.Fatal(err)
		}

		mineBlocks(t, cm, 1)
		ms.Sync(t, cm)

		txn2 := types.V2Transaction{
			Attestations: []types.Attestation{
				ann.ToAttestation(cm.TipState(), sk),
			},
		}

		if _, err := cm.AddV2PoolTransactions(cm.Tip(), []types.V2Transaction{txn2}); err != nil {
			t.Fatal(err)
		}

		mineBlocks(t, cm, 1)
		ms.Sync(t, cm)
	})

	t.Run("arbitrary data + attestation", func(t *testing.T) {
		store, tipState, err := chain.NewDBStore(chain.NewMemDB(), n, genesisBlock, nil)
		if err != nil {
			t.Fatal(err)
		}
		cm := chain.NewManager(store, tipState)
		ms := newMemState()

		// mine until a utxo is spendable
		mineBlocks(t, cm, int(n.MaturityDelay)+1)
		ms.Sync(t, cm)

		sk := types.GeneratePrivateKey()
		ann := chain.V2HostAnnouncement{
			{Address: "foo.bar:1234", Protocol: "tcp"},
		}
		txn := types.V2Transaction{
			ArbitraryData: frand.Bytes(16),
			Attestations: []types.Attestation{
				ann.ToAttestation(cm.TipState(), sk),
			},
		}

		if _, err := cm.AddV2PoolTransactions(cm.Tip(), []types.V2Transaction{txn}); err != nil {
			t.Fatal(err)
		}

		mineBlocks(t, cm, 1)
		ms.Sync(t, cm)

		txn2 := types.V2Transaction{
			Attestations: []types.Attestation{
				ann.ToAttestation(cm.TipState(), sk),
			},
		}

		if _, err := cm.AddV2PoolTransactions(cm.Tip(), []types.V2Transaction{txn2}); err != nil {
			t.Fatal(err)
		}

		mineBlocks(t, cm, 1)
		ms.Sync(t, cm)
	})

	t.Run("arbitrary data + attestation + change output", func(t *testing.T) {
		store, tipState, err := chain.NewDBStore(chain.NewMemDB(), n, genesisBlock, nil)
		if err != nil {
			t.Fatal(err)
		}
		cm := chain.NewManager(store, tipState)
		ms := newMemState()

		// mine until a utxo is spendable
		mineBlocks(t, cm, int(n.MaturityDelay)+1)
		ms.Sync(t, cm)

		sk := types.GeneratePrivateKey()
		ann := chain.V2HostAnnouncement{
			{Address: "foo.bar:1234", Protocol: "tcp"},
		}
		se := ms.SpendableElement(t)
		minerFee := types.BigFiles(1)
		txn := types.V2Transaction{
			BigFileInputs: []types.V2BigFileInput{
				{Parent: se.Copy(), SatisfiedPolicy: types.SatisfiedPolicy{Policy: policy}},
			},
			BigFileOutputs: []types.BigFileOutput{
				{Address: addr, Value: se.BigFileOutput.Value.Sub(minerFee)},
			},
			MinerFee:      minerFee,
			ArbitraryData: frand.Bytes(16),
			Attestations: []types.Attestation{
				ann.ToAttestation(cm.TipState(), sk),
			},
		}

		if _, err := cm.AddV2PoolTransactions(cm.Tip(), []types.V2Transaction{txn}); err != nil {
			t.Fatal(err)
		}

		mineBlocks(t, cm, 1)
		ms.Sync(t, cm)

		txn2 := types.V2Transaction{
			Attestations: []types.Attestation{
				ann.ToAttestation(cm.TipState(), sk),
			},
		}

		if _, err := cm.AddV2PoolTransactions(cm.Tip(), []types.V2Transaction{txn2}); err != nil {
			t.Fatal(err)
		}

		mineBlocks(t, cm, 1)
		ms.Sync(t, cm)
	})
}
