package chain_test

import (
	"slices"
	"strings"
	"testing"

	"go.thebigfile.com/core/types"
	"go.thebigfile.com/coreutils/chain"
	"go.thebigfile.com/coreutils/testutil"
)

func TestAddV2PoolTransactionsRecover(t *testing.T) {
	n, genesisBlock := testutil.V2Network()

	sk := types.GeneratePrivateKey()
	sp := types.PolicyPublicKey(sk.PublicKey())
	addr := sp.Address()

	store, genesisState, err := chain.NewDBStore(chain.NewMemDB(), n, genesisBlock, nil)
	if err != nil {
		t.Fatal(err)
	}
	cm := chain.NewManager(store, genesisState)
	es := testutil.NewElementStateStore(t, cm)

	testutil.MineBlocks(t, cm, addr, 20+int(n.MaturityDelay))
	es.Wait(t)

	cs := cm.TipState()
	basis, biges := es.BigfileElements()

	// pick the earliest element to use for the corrupt transaction
	i := -1
	var selected types.BigfileElement
	for j, bige := range biges {
		if bige.BigfileOutput.Address != addr || bige.MaturityHeight > cs.Index.Height {
			continue
		}
		if i == -1 || bige.StateElement.LeafIndex < selected.StateElement.LeafIndex {
			i = j
			selected = bige
		}
	}
	if i == -1 {
		t.Fatal("no valid BigfileElement found")
	}
	biges = slices.Delete(biges, i, i)

	// spend all the other utxos to create a large tree diff
	for _, bige := range biges {
		if bige.BigfileOutput.Address != addr || bige.MaturityHeight > cs.Index.Height {
			continue
		}

		txn := types.V2Transaction{
			BigfileInputs: []types.V2BigfileInput{
				{
					Parent: bige,
					SatisfiedPolicy: types.SatisfiedPolicy{
						Policy: sp,
					},
				},
			},
			MinerFee: types.Bigfiles(1),
			BigfileOutputs: []types.BigfileOutput{
				{
					Address: addr,
					Value:   bige.BigfileOutput.Value.Sub(types.Bigfiles(1)),
				},
			},
		}
		sigHash := cs.InputSigHash(txn)
		txn.BigfileInputs[0].SatisfiedPolicy.Signatures = []types.Signature{sk.SignHash(sigHash)}

		if _, err := cm.AddV2PoolTransactions(basis, []types.V2Transaction{txn}); err != nil {
			t.Fatal(err)
		}
	}

	// create a transaction with the selected element but corrupt its proof
	selected.StateElement.MerkleProof = selected.StateElement.MerkleProof[1:]
	txn := types.V2Transaction{
		BigfileInputs: []types.V2BigfileInput{
			{
				Parent: selected,
				SatisfiedPolicy: types.SatisfiedPolicy{
					Policy: sp,
				},
			},
		},
		BigfileOutputs: []types.BigfileOutput{
			{
				Address: types.VoidAddress,
				Value:   selected.BigfileOutput.Value,
			},
		},
	}
	sigHash := cs.InputSigHash(txn)
	txn.BigfileInputs[0].SatisfiedPolicy.Signatures = []types.Signature{sk.SignHash(sigHash)}

	// mine blocks to require an update
	testutil.MineBlocks(t, cm, addr, 20)
	es.Wait(t)

	if _, err := cm.AddV2PoolTransactions(basis, []types.V2Transaction{txn}); err == nil || !strings.Contains(err.Error(), "invalid Merkle proof") {
		t.Fatalf("expected invalid transaction, got %v", err)
	}
}
