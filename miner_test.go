package coreutils_test

import (
	"testing"
	"time"

	"go.thebigfile.com/core/types"
	"go.thebigfile.com/coreutils"
	"go.thebigfile.com/coreutils/chain"
	"go.thebigfile.com/coreutils/testutil"
)

func TestMiner(t *testing.T) {
	n, genesisBlock := testutil.Network()

	sk := types.GeneratePrivateKey()
	genesisBlock.Transactions = []types.Transaction{{
		BigfileOutputs: []types.BigfileOutput{
			{
				Address: types.StandardUnlockHash(sk.PublicKey()),
				Value:   types.Bigfiles(10),
			},
		},
	}}

	store, tipState, err := chain.NewDBStore(chain.NewMemDB(), n, genesisBlock, nil)
	if err != nil {
		t.Fatal(err)
	}
	cm := chain.NewManager(store, tipState)

	// create a transaction
	txn := types.Transaction{
		BigfileInputs: []types.BigfileInput{{
			ParentID:         genesisBlock.Transactions[0].BigfileOutputID(0),
			UnlockConditions: types.StandardUnlockConditions(sk.PublicKey()),
		}},
		BigfileOutputs: []types.BigfileOutput{{
			Address: types.StandardUnlockHash(sk.PublicKey()),
			Value:   types.Bigfiles(9),
		}},
		MinerFees: []types.Currency{types.Bigfiles(1)},
	}

	// sign the inputs
	for _, bigi := range txn.BigfileInputs {
		sig := sk.SignHash(cm.TipState().WholeSigHash(txn, types.Hash256(bigi.ParentID), 0, 0, nil))
		txn.Signatures = append(txn.Signatures, types.TransactionSignature{
			ParentID:       types.Hash256(bigi.ParentID),
			CoveredFields:  types.CoveredFields{WholeTransaction: true},
			PublicKeyIndex: 0,
			Signature:      sig[:],
		})
	}

	// add the transaction to the pool
	_, err = cm.AddPoolTransactions([]types.Transaction{txn})
	if err != nil {
		t.Fatal(err)
	}

	// assert the minerpayout includes the txn fee
	expectedPayout := types.Bigfiles(1).Add(cm.TipState().BlockReward())
	b, found := coreutils.MineBlock(cm, types.VoidAddress, time.Second)
	if !found {
		t.Fatal("PoW failed")
	} else if len(b.MinerPayouts) != 1 {
		t.Fatal("expected one miner payout")
	} else if b.MinerPayouts[0].Value.Cmp(expectedPayout) != 0 {
		t.Fatalf("expected miner payout %d, got %d", expectedPayout, b.MinerPayouts[0].Value)
	}
}

func TestV2MineBlocks(t *testing.T) {
	n, genesisBlock := testutil.V2Network()
	n.HardforkV2.AllowHeight = 5
	n.HardforkV2.RequireHeight = 10
	n.InitialTarget = types.BlockID{0xFF}

	genesisBlock.Transactions = []types.Transaction{{
		BigfileOutputs: []types.BigfileOutput{
			{
				Address: types.AnyoneCanSpend().Address(),
				Value:   types.Bigfiles(10),
			},
		},
	}}

	store, tipState, err := chain.NewDBStore(chain.NewMemDB(), n, genesisBlock, nil)
	if err != nil {
		t.Fatal(err)
	}
	cm := chain.NewManager(store, tipState)

	mineBlocks := func(t *testing.T, n int) {
		for ; n > 0; n-- {
			b, ok := coreutils.MineBlock(cm, types.VoidAddress, time.Second)
			if !ok {
				t.Fatal("failed to mine block")
			} else if err := cm.AddBlocks([]types.Block{b}); err != nil {
				t.Fatal(err)
			}
		}
	}

	// mine until just before the allow height
	mineBlocks(t, 4)

	elements := make(map[types.BigfileOutputID]types.BigfileElement)
	_, applied, err := cm.UpdatesSince(types.ChainIndex{}, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, cau := range applied {
		for _, biged := range cau.BigfileElementDiffs() {
			bige := biged.BigfileElement
			if bige.BigfileOutput.Address == types.AnyoneCanSpend().Address() {
				if biged.Created {
					elements[bige.ID] = bige
				}
				if biged.Spent {
					delete(elements, bige.ID)
				}
			}
		}
		for k, v := range elements {
			cau.UpdateElementProof(&v.StateElement)
			elements[k] = v
		}
	}

	var se types.BigfileElement
	for _, v := range elements {
		se = v
		break
	}

	txn := types.V2Transaction{
		MinerFee: se.BigfileOutput.Value,
		BigfileInputs: []types.V2BigfileInput{{
			Parent:          se,
			SatisfiedPolicy: types.SatisfiedPolicy{Policy: types.AnyoneCanSpend()},
		}},
	}

	// add the transaction to the pool
	_, err = cm.AddV2PoolTransactions(cm.Tip(), []types.V2Transaction{txn})
	if err != nil {
		t.Fatal(err)
	}

	mineBlocks(t, 1)
}
