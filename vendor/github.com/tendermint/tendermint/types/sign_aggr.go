package types

import (
	//"bytes"
	//"errors"
	"fmt"
	//"io"
	"strings"

	. "github.com/tendermint/go-common"
	"github.com/tendermint/go-wire"
	//"github.com/tendermint/go-data"
)

//------------------------ signature aggregation -------------------
const MaxSignAggrSize = 22020096 // 21MB TODO make it configurable

type SignAggr struct {
	Height           int              `json:"height"`
	Round            int              `json:"round"`
	Type             byte             `json:"type"`
	NumValidators	 int              `json:"numValidators"`
	BlockID          BlockID          `json:"block_id"` // zero if vote is nil.
        BitArray         *BitArray         // valIndex -> hasVote?
	
	// BLS signature aggregation to be added here
	SignAggr	BLSSignature

	sum		int64             // Sum of voting power for seen votes, discounting conflicts
	maj23		*BlockID	  // First 2/3 majority seen
}

func MakeSignAggr(height int, round int, mtype byte, numValidators int, blockID BlockID, signAggr BLSSignature) *SignAggr {
        return &SignAggr{
		Height	: height,
		Round	: round,
		Type	: mtype,
		NumValidators: numValidators,
		BlockID	: blockID
                BitArray: NewBitArray(numValidators),
		SignAggr : signAggr,
		sum	: 0,
		maj23	: nil,
        }
}

func (sa *SignAggr) SignAggr() BLSSignature {
	return sa.SignAggr
}

func (sa *SignAggr) HasTwoThirdsMajority() bool {
	if sa == nil {
		return false
	}
	return sa.maj23 != nil
}

func (sa *SignAggr) IsCommit() bool {
	if sa == nil {
		return false
	}
	if sa.type_ != VoteTypePrecommit {
		return false
	}
	sa.mtx.Lock()
	defer sa.mtx.Unlock()
	return sa.maj23 != nil
}

/*
func (sa *SignAggr) HasTwoThirdsAny() bool {
	if sa == nil {
		return false
	}
	sa.mtx.Lock()
	defer sa.mtx.Unlock()
	return sa.sum > voteSet.valSet.TotalVotingPower()*2/3
}

func (sa *SignAggr) HasAll() bool {
	return sa.sum == sa.valSet.TotalVotingPower()
}
*/

// Returns either a blockhash (or nil) that received +2/3 majority.
// If there exists no such majority, returns (nil, PartSetHeader{}, false).
func (sa *SignAggr) TwoThirdsMajority() (blockID BlockID, ok bool) {
	if sa == nil {
		return BlockID{}, false
	}
	if sa.maj23 != nil {
		return *sa.maj23, true
	} else {
		return BlockID{}, false
	}
}

func (va *SignAggr) String() string {
	return va.StringIndented("")
}

func (va *SignAggr) StringIndented(indent string) string {
	if va == nil {
		return "nil-SignAggr"
	}
	return fmt.Sprintf(`SignAggr{
%s  %v
%s  %v
%s  %v
%s  %v
}`,
		indent, va.Height,
		indent, va.Round,
		indent, va.Type,
		indent, va.NumValidators)
}