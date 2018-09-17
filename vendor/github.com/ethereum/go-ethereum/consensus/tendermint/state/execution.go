package state

import (
	"github.com/ethereum/go-ethereum/consensus/tendermint/types"
	"github.com/pkg/errors"
	ethTypes "github.com/ethereum/go-ethereum/core/types"
	. "github.com/tendermint/go-common"
	ep "github.com/ethereum/go-ethereum/consensus/tendermint/epoch"
)

//--------------------------------------------------

// return a bit array of validators that signed the last commit
// NOTE: assumes commits have already been authenticated
/*
func commitBitArrayFromBlock(block *types.TdmBlock) *BitArray {

	signed := NewBitArray(uint64(len(block.TdmExtra.SeenCommit.Precommits)))
	for i, precommit := range block.TdmExtra.SeenCommit.Precommits {
		if precommit != nil {
			signed.SetIndex(uint64(i), true) // val_.LastCommitHeight = block.Height - 1
		}
	}
	return signed
}*/

//-----------------------------------------------------
// Validate block

func (s *State) ValidateBlock(block *types.TdmBlock) error {
	return s.validateBlock(block)
}

//Very current block
func (s *State) validateBlock(block *types.TdmBlock) error {
	// Basic block validation.
	err := block.ValidateBasic(s.TdmExtra)
	if err != nil {
		return err
	}

	// Validate block SeenCommit.
	epoch := s.Epoch.GetEpochByBlockNumber(block.TdmExtra.Height)
	if epoch == nil || epoch.Validators == nil {
		return errors.New("no epoch for current block height")
	}

	valSet := epoch.Validators
	err = valSet.VerifyCommit(block.TdmExtra.ChainID, block.TdmExtra.Height,
		block.TdmExtra.SeenCommit)
	if err != nil {
		return err
	}

	return nil
}

//-----------------------------------------------------------------------------
// ApplyBlock applies the epoch infor from last block
func (s *State) ApplyBlock(block *ethTypes.Block, epoch *ep.Epoch) *ep.Epoch {

	if block.NumberU64() == 0 {
		return epoch
	}

	tdmExtra, _ := types.ExtractTendermintExtra(block.Header())
	//here handles the proposed next epoch
	nextEpochInBlock := ep.FromBytes(tdmExtra.EpochBytes)
	if nextEpochInBlock != nil {
		epoch.SetNextEpoch(nextEpochInBlock)
		epoch.NextEpoch.RS = s.Epoch.RS
		epoch.NextEpoch.Status = ep.EPOCH_VOTED_NOT_SAVED
		epoch.Save()
	}

	//here handles if need to enter next epoch
	ok, err := epoch.ShouldEnterNewEpoch(tdmExtra.Height)
	if ok && err == nil {
		// now update the block and validators
		epoch, _, _ := epoch.EnterNewEpoch(tdmExtra.Height)
		epoch.Save()
	} else if err != nil {
		logger.Error(Fmt("ApplyBlock(%v): Invalid epoch. Current epoch: %v, error: %v",
			tdmExtra.Height, s.Epoch, err))
		return nil
	}

	return epoch
}
