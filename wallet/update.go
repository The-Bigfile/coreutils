package wallet

import (
	"fmt"
	"time"

	"go.thebigfile.com/core/types"
	"go.thebigfile.com/coreutils/chain"
)

type (
	// A ChainUpdate is an interface for iterating over the elements in a chain
	// update.
	ChainUpdate interface {
		ForEachBigfileElement(func(bige types.BigfileElement, spent bool))
		ForEachBigfundElement(func(bfe types.BigfundElement, spent bool))
		ForEachFileContractElement(func(fce types.FileContractElement, rev *types.FileContractElement, resolved, valid bool))
		ForEachV2FileContractElement(func(fce types.V2FileContractElement, rev *types.V2FileContractElement, res types.V2FileContractResolutionType))
	}

	// A ProofUpdater is an interface for updating the proof of a state element.
	ProofUpdater interface {
		UpdateElementProof(e *types.StateElement)
	}

	// UpdateTx is an interface for atomically applying chain updates to a
	// single address wallet.
	UpdateTx interface {
		// UpdateWalletBigfileElementProofs updates the proofs of all state elements
		// affected by the update. ProofUpdater.UpdateElementProof must be called
		// for each state element in the database.
		UpdateWalletBigfileElementProofs(ProofUpdater) error

		// WalletApplyIndex is called with the chain index that is being applied.
		// Any transactions and bigfile elements that were created by the index
		// should be added and any bigfile elements that were spent should be
		// removed.
		//
		// timestamp is the timestamp of the block being applied.
		WalletApplyIndex(index types.ChainIndex, created, spent []types.BigfileElement, events []Event, timestamp time.Time) error
		// WalletRevertIndex is called with the chain index that is being reverted.
		// Any transactions that were added by the index should be removed
		//
		// removed contains the bigfile elements that were created by the index
		// and should be deleted.
		//
		// unspent contains the bigfile elements that were spent and should be
		// recreated. They are not necessarily created by the index and should
		// not be associated with it.
		//
		// timestamp is the timestamp of the block being reverted
		WalletRevertIndex(index types.ChainIndex, removed, unspent []types.BigfileElement, timestamp time.Time) error
	}
)

// relevantV1Txn returns true if the transaction is relevant to the provided address
func relevantV1Txn(txn types.Transaction, addr types.Address) bool {
	for _, so := range txn.BigfileOutputs {
		if so.Address == addr {
			return true
		}
	}
	for _, si := range txn.BigfileInputs {
		if si.UnlockConditions.UnlockHash() == addr {
			return true
		}
	}
	return false
}

func relevantV2Txn(txn types.V2Transaction, addr types.Address) bool {
	for _, so := range txn.BigfileOutputs {
		if so.Address == addr {
			return true
		}
	}
	for _, si := range txn.BigfileInputs {
		if si.Parent.BigfileOutput.Address == addr {
			return true
		}
	}
	return false
}

// appliedEvents returns a slice of events that are relevant to the wallet
// in the chain update.
func appliedEvents(cau chain.ApplyUpdate, walletAddress types.Address) (events []Event) {
	cs := cau.State
	block := cau.Block
	index := cs.Index
	bigfileElements := make(map[types.BigfileOutputID]types.BigfileElement)

	// cache the value of bigfile elements to use when calculating v1 outflow
	for _, biged := range cau.BigfileElementDiffs() {
		biged.BigfileElement.StateElement.MerkleProof = nil // clear the proof to save space
		bigfileElements[biged.BigfileElement.ID] = biged.BigfileElement.Move()
	}

	addEvent := func(id types.Hash256, eventType string, data EventData, maturityHeight uint64) {
		ev := Event{
			ID:             id,
			Index:          index,
			Data:           data,
			Type:           eventType,
			Timestamp:      block.Timestamp,
			MaturityHeight: maturityHeight,
			Relevant:       []types.Address{walletAddress},
		}

		if ev.BigfileInflow().Equals(ev.BigfileOutflow()) {
			// skip events that don't affect the wallet
			return
		}
		events = append(events, ev)
	}

	for _, txn := range block.Transactions {
		if !relevantV1Txn(txn, walletAddress) {
			continue
		}
		for _, si := range txn.BigfundInputs {
			if si.UnlockConditions.UnlockHash() == walletAddress {
				outputID := si.ParentID.ClaimOutputID()
				bige, ok := bigfileElements[outputID]
				if !ok {
					panic("missing claim bigfile element")
				}

				addEvent(types.Hash256(outputID), EventTypeBigfundClaim, EventPayout{
					BigfileElement: bige.Copy(),
				}, bige.MaturityHeight)
			}
		}

		event := EventV1Transaction{
			Transaction: txn,
		}

		for _, si := range txn.BigfileInputs {
			se, ok := bigfileElements[types.BigfileOutputID(si.ParentID)]
			if !ok {
				panic("missing transaction bigfile element")
			} else if se.BigfileOutput.Address != walletAddress {
				continue
			}
			event.SpentBigfileElements = append(event.SpentBigfileElements, se.Copy())
		}
		addEvent(types.Hash256(txn.ID()), EventTypeV1Transaction, event, index.Height)
	}

	for _, txn := range block.V2Transactions() {
		if !relevantV2Txn(txn, walletAddress) {
			continue
		}
		for _, si := range txn.BigfundInputs {
			if si.Parent.BigfundOutput.Address == walletAddress {
				outputID := types.BigfundOutputID(si.Parent.ID).V2ClaimOutputID()
				bige, ok := bigfileElements[outputID]
				if !ok {
					panic("missing claim bigfile element")
				}

				addEvent(types.Hash256(outputID), EventTypeBigfundClaim, EventPayout{
					BigfileElement: bige.Copy(),
				}, bige.MaturityHeight)
			}
		}

		addEvent(types.Hash256(txn.ID()), EventTypeV2Transaction, EventV2Transaction(txn), index.Height)
	}

	// add the file contract outputs
	for _, fced := range cau.FileContractElementDiffs() {
		if !fced.Resolved {
			continue
		}
		fced.FileContractElement.StateElement.MerkleProof = nil // clear the proof to save space
		fce := fced.FileContractElement.Move()

		if fced.Valid {
			for i, so := range fce.FileContract.ValidProofOutputs {
				if so.Address != walletAddress {
					continue
				}

				outputID := fce.ID.ValidOutputID(i)
				bige, ok := bigfileElements[outputID]
				if !ok {
					panic("missing bigfile element")
				}

				addEvent(types.Hash256(outputID), EventTypeV1ContractResolution, EventV1ContractResolution{
					Parent:         fce.Copy(),
					BigfileElement: bige.Copy(),
					Missed:         false,
				}, bige.MaturityHeight)
			}
		} else {
			for i, so := range fce.FileContract.MissedProofOutputs {
				if so.Address != walletAddress {
					continue
				}

				outputID := fce.ID.MissedOutputID(i)
				bige, ok := bigfileElements[outputID]
				if !ok {
					panic("missing bigfile element")
				}

				addEvent(types.Hash256(outputID), EventTypeV1ContractResolution, EventV1ContractResolution{
					Parent:         fce.Copy(),
					BigfileElement: bige.Copy(),
					Missed:         true,
				}, bige.MaturityHeight)
			}
		}
	}

	for _, fced := range cau.V2FileContractElementDiffs() {
		if fced.Resolution == nil {
			continue
		}
		fced.V2FileContractElement.StateElement.MerkleProof = nil // clear the proof to save space
		fce := fced.V2FileContractElement.Move()

		_, missed := fced.Resolution.(*types.V2FileContractExpiration)
		if fce.V2FileContract.HostOutput.Address == walletAddress {
			outputID := fce.ID.V2HostOutputID()
			bige, ok := bigfileElements[outputID]
			if !ok {
				panic("missing bigfile element")
			}

			addEvent(types.Hash256(outputID), EventTypeV2ContractResolution, EventV2ContractResolution{
				Resolution: types.V2FileContractResolution{
					Parent:     fce.Copy(),
					Resolution: fced.Resolution,
				},
				BigfileElement: bige.Copy(),
				Missed:         missed,
			}, bige.MaturityHeight)
		}

		if fce.V2FileContract.RenterOutput.Address == walletAddress {
			outputID := fce.ID.V2RenterOutputID()
			bige, ok := bigfileElements[outputID]
			if !ok {
				panic("missing bigfile element")
			}

			addEvent(types.Hash256(outputID), EventTypeV2ContractResolution, EventV2ContractResolution{
				Resolution: types.V2FileContractResolution{
					Parent:     fce.Copy(),
					Resolution: fced.Resolution,
				},
				BigfileElement: bige.Copy(),
				Missed:         missed,
			}, bige.MaturityHeight)
		}
	}

	blockID := block.ID()
	for i, so := range block.MinerPayouts {
		if so.Address != walletAddress {
			continue
		}

		outputID := blockID.MinerOutputID(i)
		bige, ok := bigfileElements[outputID]
		if !ok {
			panic("missing bigfile element")
		}
		addEvent(types.Hash256(outputID), EventTypeMinerPayout, EventPayout{
			BigfileElement: bige.Copy(),
		}, bige.MaturityHeight)
	}

	outputID := blockID.FoundationOutputID()
	if bige, ok := bigfileElements[outputID]; ok && bige.BigfileOutput.Address == walletAddress {
		addEvent(types.Hash256(outputID), EventTypeFoundationSubsidy, EventPayout{
			BigfileElement: bige.Copy(),
		}, bige.MaturityHeight)
	}
	return
}

// applyChainUpdate atomically applies a chain update
func (sw *SingleAddressWallet) applyChainUpdate(tx UpdateTx, address types.Address, cau chain.ApplyUpdate) error {
	// update current state elements
	if err := tx.UpdateWalletBigfileElementProofs(cau); err != nil {
		return fmt.Errorf("failed to update state elements: %w", err)
	}

	var createdUTXOs, spentUTXOs []types.BigfileElement
	for _, biged := range cau.BigfileElementDiffs() {
		switch {
		case biged.Created && biged.Spent:
			continue // ignore ephemeral elements
		case biged.BigfileElement.BigfileOutput.Address != address:
			continue // ignore elements that are not related to the wallet
		case biged.Created:
			createdUTXOs = append(createdUTXOs, biged.BigfileElement.Share())
		case biged.Spent:
			spentUTXOs = append(spentUTXOs, biged.BigfileElement.Share())
		default:
			panic("unexpected bigfile element") // developer error
		}
	}

	if err := tx.WalletApplyIndex(cau.State.Index, createdUTXOs, spentUTXOs, appliedEvents(cau, address), cau.Block.Timestamp); err != nil {
		return fmt.Errorf("failed to apply index: %w", err)
	}
	return nil
}

// revertChainUpdate atomically reverts a chain update from a wallet
func (sw *SingleAddressWallet) revertChainUpdate(tx UpdateTx, revertedIndex types.ChainIndex, address types.Address, cru chain.RevertUpdate) error {
	var removedUTXOs, unspentUTXOs []types.BigfileElement
	for _, biged := range cru.BigfileElementDiffs() {
		switch {
		case biged.Created && biged.Spent:
			continue // ignore ephemeral elements
		case biged.BigfileElement.BigfileOutput.Address != address:
			continue // ignore elements that are not related to the wallet
		case biged.Spent:
			unspentUTXOs = append(unspentUTXOs, biged.BigfileElement.Share())
		case biged.Created:
			removedUTXOs = append(removedUTXOs, biged.BigfileElement.Share())
		default:
			panic("unexpected bigfile element") // developer error
		}
	}

	// remove any existing events that were added in the reverted block
	if err := tx.WalletRevertIndex(revertedIndex, removedUTXOs, unspentUTXOs, cru.Block.Timestamp); err != nil {
		return fmt.Errorf("failed to revert block: %w", err)
	}

	// update the remaining state elements
	if err := tx.UpdateWalletBigfileElementProofs(cru); err != nil {
		return fmt.Errorf("failed to update state elements: %w", err)
	}
	return nil
}

// UpdateChainState atomically applies and reverts chain updates to a single
// wallet store.
func (sw *SingleAddressWallet) UpdateChainState(tx UpdateTx, reverted []chain.RevertUpdate, applied []chain.ApplyUpdate) error {
	for _, cru := range reverted {
		revertedIndex := types.ChainIndex{
			ID:     cru.Block.ID(),
			Height: cru.State.Index.Height + 1,
		}
		err := sw.revertChainUpdate(tx, revertedIndex, sw.addr, cru)
		if err != nil {
			return fmt.Errorf("failed to revert chain update %q: %w", cru.State.Index, err)
		}
	}

	for _, cau := range applied {
		err := sw.applyChainUpdate(tx, sw.addr, cau)
		if err != nil {
			return fmt.Errorf("failed to apply chain update %q: %w", cau.State.Index, err)
		}
	}
	return nil
}
