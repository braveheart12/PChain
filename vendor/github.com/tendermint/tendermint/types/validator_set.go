package types

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"

	abci "github.com/tendermint/abci/types"
	cmn "github.com/tendermint/go-common"
	crypto "github.com/tendermint/go-crypto"
	"github.com/tendermint/go-merkle"
	"github.com/tendermint/go-wire"
)

// ValidatorSet represent a set of *Validator at a given height.
// The validators can be fetched by address or index.
// The index is in order of .Address, so the indices are fixed
// for all rounds of a given blockchain height.
// On the other hand, the .AccumPower of each validator and
// the designated .GetProposer() of a set changes every round,
// upon calling .IncrementAccum().
// NOTE: Not goroutine-safe.
// NOTE: All get/set to validators should copy the value for safety.
// TODO: consider validator Accum overflow
// TODO: move valset into an iavl tree where key is 'blockbonded|pubkey'
type ValidatorSet struct {
	// NOTE: persisted via reflect, must be exported.
	Validators []*Validator `json:"validators"`
	Proposer   *Validator   `json:"proposer"`

	// cached (unexported)
	totalVotingPower *big.Int
}

func NewValidatorSet(vals []*Validator) *ValidatorSet {
	validators := make([]*Validator, len(vals))
	for i, val := range vals {
		validators[i] = val.Copy()
	}
	sort.Sort(ValidatorsByAddress(validators))
	vs := &ValidatorSet{
		Validators: validators,
	}

	if vals != nil {
		vs.IncrementAccum(1)
	}

	return vs
}

// incrementAccum and update the proposer
// TODO: mind the overflow when times and votingPower shares too large.
func (valSet *ValidatorSet) IncrementAccum(times int) {
	// Add VotingPower * times to each validator and order into heap.
	validatorsHeap := cmn.NewHeap()
	for _, val := range valSet.Validators {
		val.Accum.Add(val.Accum, new(big.Int).Mul(val.VotingPower, big.NewInt(int64(times)))) // TODO: mind overflow
		validatorsHeap.Push(val, accumComparable{val})
	}

	// Decrement the validator with most accum times times
	for i := 0; i < times; i++ {
		mostest := validatorsHeap.Peek().(*Validator)
		if i == times-1 {
			valSet.Proposer = mostest
		}
		mostest.Accum.Sub(mostest.Accum, valSet.TotalVotingPower())
		validatorsHeap.Update(mostest, accumComparable{mostest})
	}
}

func (valSet *ValidatorSet) Copy() *ValidatorSet {
	validators := make([]*Validator, len(valSet.Validators))
	for i, val := range valSet.Validators {
		// NOTE: must copy, since IncrementAccum updates in place.
		validators[i] = val.Copy()
	}
	return &ValidatorSet{
		Validators:       validators,
		Proposer:         valSet.Proposer,
		totalVotingPower: valSet.totalVotingPower,
	}
}

func (valSet *ValidatorSet) AggrPubKey(bitMap *cmn.BitArray) crypto.PubKey {
	if bitMap == nil {
		return nil
	}
	if bitMap.Size() != len(valSet.Validators) {
		return nil
	}
	validators := valSet.Validators
	var pks []*crypto.PubKey
	for i := 0; i < bitMap.Size(); i++ {
		if bitMap.GetIndex(i) {
			pks = append(pks, &(validators[i].PubKey))
		}
	}
	return crypto.BLSPubKeyAggregate(pks)
}

func (valSet *ValidatorSet) TalliedVotingPower(bitMap *cmn.BitArray) (*big.Int, error) {
	if bitMap == nil {
		return big.NewInt(0),fmt.Errorf("Invalid bitmap(nil)")
	}
	validators := valSet.Validators
	if validators == nil {
		return big.NewInt(0), fmt.Errorf("Invalid validators(nil)")
	}
	if valSet.Size() != bitMap.Size() {
		return big.NewInt(0), fmt.Errorf("Size is not equal, validators size:%v, bitmap size:%v", valSet.Size(), bitMap.Size())
	}
	powerSum := big.NewInt(0)
	for i := 0; i < bitMap.Size(); i++ {
		powerSum.Add(powerSum,validators[i].VotingPower )
	}
	return powerSum,nil
}

func (valSet *ValidatorSet) Equals(other *ValidatorSet) bool {

	if valSet.totalVotingPower.Cmp(other.totalVotingPower) != 0 ||
		!valSet.Proposer.Equals(other.Proposer) ||
		len(valSet.Validators) != len(other.Validators) {
		return false
	}

	for _, v := range other.Validators {

		_, val := valSet.GetByAddress(v.Address)
		if val == nil || !val.Equals(v) {
			return false
		}
	}

	return true
}

// HasAddress returns true if address given is in the validator set, false -
// otherwise.
func (valSet *ValidatorSet) HasAddress(address []byte) bool {
	idx := sort.Search(len(valSet.Validators), func(i int) bool {
		return bytes.Compare(address, valSet.Validators[i].Address) <= 0
	})
	return idx < len(valSet.Validators) && bytes.Equal(valSet.Validators[idx].Address, address)
}

func (valSet *ValidatorSet) GetByAddress(address []byte) (index int, val *Validator) {
	idx := sort.Search(len(valSet.Validators), func(i int) bool {
		return bytes.Compare(address, valSet.Validators[i].Address) <= 0
	})
	if idx != len(valSet.Validators) && bytes.Compare(valSet.Validators[idx].Address, address) == 0 {
		return idx, valSet.Validators[idx].Copy()
	} else {
		return 0, nil
	}
}

func (valSet *ValidatorSet) GetByIndex(index int) (address []byte, val *Validator) {
	val = valSet.Validators[index]
	return val.Address, val.Copy()
}

func (valSet *ValidatorSet) Size() int {
	return len(valSet.Validators)
}

func (valSet *ValidatorSet) TotalVotingPower() *big.Int {
	if valSet.totalVotingPower == nil {
		valSet.totalVotingPower = big.NewInt(0)
		for _, val := range valSet.Validators {
			valSet.totalVotingPower.Add(valSet.totalVotingPower, val.VotingPower)
		}
	}
	return valSet.totalVotingPower
}

func (valSet *ValidatorSet) GetProposer() (proposer *Validator) {
	if len(valSet.Validators) == 0 {
		return nil
	}
	if valSet.Proposer == nil {
		valSet.Proposer = valSet.findProposer()
	}
	return valSet.Proposer.Copy()
}

func (valSet *ValidatorSet) findProposer() *Validator {
	var proposer *Validator
	for _, val := range valSet.Validators {
		if proposer == nil || !bytes.Equal(val.Address, proposer.Address) {
			proposer = proposer.CompareAccum(val)
		}
	}
	return proposer
}

func (valSet *ValidatorSet) Hash() []byte {
	if len(valSet.Validators) == 0 {
		return nil
	}
	hashables := make([]merkle.Hashable, len(valSet.Validators))
	for i, val := range valSet.Validators {
		hashables[i] = val
	}
	return merkle.SimpleHashFromHashables(hashables)
}

func (valSet *ValidatorSet) Add(val *Validator) (added bool) {
	val = val.Copy()
	idx := sort.Search(len(valSet.Validators), func(i int) bool {
		return bytes.Compare(val.Address, valSet.Validators[i].Address) <= 0
	})
	if idx == len(valSet.Validators) {
		valSet.Validators = append(valSet.Validators, val)
		// Invalidate cache
		valSet.Proposer = nil
		valSet.totalVotingPower = nil
		return true
	} else if bytes.Compare(valSet.Validators[idx].Address, val.Address) == 0 {
		return false
	} else {
		newValidators := make([]*Validator, len(valSet.Validators)+1)
		copy(newValidators[:idx], valSet.Validators[:idx])
		newValidators[idx] = val
		copy(newValidators[idx+1:], valSet.Validators[idx:])
		valSet.Validators = newValidators
		// Invalidate cache
		valSet.Proposer = nil
		valSet.totalVotingPower = nil
		return true
	}
}

func (valSet *ValidatorSet) Update(val *Validator) (updated bool) {
	index, sameVal := valSet.GetByAddress(val.Address)
	if sameVal == nil {
		return false
	} else {
		valSet.Validators[index] = val.Copy()
		// Invalidate cache
		valSet.Proposer = nil
		valSet.totalVotingPower = nil
		return true
	}
}

func (valSet *ValidatorSet) Remove(address []byte) (val *Validator, removed bool) {
	idx := sort.Search(len(valSet.Validators), func(i int) bool {
		return bytes.Compare(address, valSet.Validators[i].Address) <= 0
	})
	if idx == len(valSet.Validators) || bytes.Compare(valSet.Validators[idx].Address, address) != 0 {
		return nil, false
	} else {
		removedVal := valSet.Validators[idx]
		newValidators := valSet.Validators[:idx]
		if idx+1 < len(valSet.Validators) {
			newValidators = append(newValidators, valSet.Validators[idx+1:]...)
		}
		valSet.Validators = newValidators
		// Invalidate cache
		valSet.Proposer = nil
		valSet.totalVotingPower = nil
		return removedVal, true
	}
}

func (valSet *ValidatorSet) Iterate(fn func(index int, val *Validator) bool) {
	for i, val := range valSet.Validators {
		stop := fn(i, val.Copy())
		if stop {
			break
		}
	}
}

// Verify that +2/3 of the set had signed the given signBytes
func (valSet *ValidatorSet) VerifyCommit(chainID string, blockID BlockID, height int, commit *Commit) error {

	fmt.Printf("(valSet *ValidatorSet) VerifyCommit(), avoid valSet and commit.Precommits size check for validatorset change\n")
	if commit == nil {
		return fmt.Errorf("Invalid commit(nil)")
	}
	if valSet.Size() != commit.BitArray.Size() {
		return fmt.Errorf("Invalid commit -- wrong set size: %v vs %v", valSet.Size(), commit.BitArray.Size())
	}
	if height != commit.Height {
		return fmt.Errorf("Invalid commit -- wrong height: %v vs %v", height, commit.Height)
	}

	pubKey := valSet.AggrPubKey(commit.BitArray)
	vote := &Vote{

		BlockID:	commit.BlockID,
		Height: 	commit.Height,
		Round: 		commit.Round,
		Type: 		commit.Type(),
	}
	if !pubKey.VerifyBytes(SignBytes(chainID, vote), commit.SignAggr) {
		return fmt.Errorf("Invalid commit -- wrong Signature:%v or BitArray:%v", commit.SignAggr, commit.BitArray)
	}

	talliedVotingPower, err := valSet.TalliedVotingPower(commit.BitArray)
	if err != nil {
		return err
	}

	quorum := big.NewInt(0)
	quorum.Mul(valSet.totalVotingPower, big.NewInt(2))
	quorum.Div(quorum, big.NewInt(3))
	quorum.Add(quorum, big.NewInt(1))
	if talliedVotingPower.Cmp(quorum) >= 0 {
		return nil
	} else {
		return fmt.Errorf("Invalid commit -- insufficient voting power: got %v, needed %v",
			talliedVotingPower, (quorum))
	}

}

// Verify that +2/3 of this set had signed the given signBytes.
// Unlike VerifyCommit(), this function can verify commits with differeent sets.
func (valSet *ValidatorSet) VerifyCommitAny(chainID string, blockID BlockID, height int, commit *Commit) error {
	panic("Not yet implemented")
	/*
			Start like:

		FOR_LOOP:
			for _, val := range vals {
				if len(precommits) == 0 {
					break FOR_LOOP
				}
				next := precommits[0]
				switch bytes.Compare(val.Address(), next.ValidatorAddress) {
				case -1:
					continue FOR_LOOP
				case 0:
					signBytes := tm.SignBytes(next)
					...
				case 1:
					... // error?
				}
			}
	*/
}

//-------------------------
//liaoyd
func UpdateValidators(validators *ValidatorSet, changedValidators []*abci.Validator) error {
	// TODO: prevent change of 1/3+ at once

	for _, v := range changedValidators {
		pubkey, err := crypto.PubKeyFromBytes(v.PubKey) // NOTE: expects go-wire encoded pubkey
		if err != nil {
			return err
		}

		address := pubkey.Address()
		power := v.Power
		// mind the overflow from uint64
		if power.Sign() == -1 {
			return errors.New(cmn.Fmt("Power (%d) overflows int64", v.Power))
		}

		_, val := validators.GetByAddress(address)
		if val == nil {
			// add val
			added := validators.Add(NewValidator(pubkey, power))
			if !added {
				return errors.New(cmn.Fmt("Failed to add new validator %X with voting power %d", address, power))
			}
		} else if v.Power.Sign() == 0 {
			// remove val
			_, removed := validators.Remove(address)
			if !removed {
				return errors.New(cmn.Fmt("Failed to remove validator %X)"))
			}
		} else {
			// update val
			val.VotingPower = power
			updated := validators.Update(val)
			if !updated {
				return errors.New(cmn.Fmt("Failed to update validator %X with voting power %d", address, power))
			}
		}
	}
	return nil
}

func (valSet *ValidatorSet) ToBytes() []byte {
	buf, n, err := new(bytes.Buffer), new(int), new(error)
	wire.WriteBinary(valSet, buf, n, err)
	if *err != nil {
		cmn.PanicCrisis(*err)
	}
	return buf.Bytes()
}

func (valSet *ValidatorSet) FromBytes(b []byte) {
	r, n, err := bytes.NewReader(b), new(int), new(error)
	wire.ReadBinary(valSet, r, 0, n, err)
	if *err != nil {
		// DATA HAS BEEN CORRUPTED OR THE SPEC HAS CHANGED
		cmn.PanicCrisis(*err)
	}
}

func (valSet *ValidatorSet) ToAbciValidators() []*abci.Validator {

	abciValidators := make([]*abci.Validator, len(valSet.Validators))
	for i, val := range valSet.Validators {
		abciValidators[i] = val.ToAbciValidator()
	}

	return abciValidators
}

func (valSet *ValidatorSet) String() string {
	return valSet.StringIndented("")
}

func (valSet *ValidatorSet) StringIndented(indent string) string {
	if valSet == nil {
		return "nil-ValidatorSet"
	}
	valStrings := []string{}
	valSet.Iterate(func(index int, val *Validator) bool {
		valStrings = append(valStrings, val.String())
		return false
	})
	return fmt.Sprintf(`ValidatorSet{
%s  Proposer: %v
%s  Validators:
%s    %v
%s}`,
		indent, valSet.GetProposer().String(),
		indent,
		indent, strings.Join(valStrings, "\n"+indent+"    "),
		indent)

}

//-------------------------------------
// Implements sort for sorting validators by address.

type ValidatorsByAddress []*Validator

func (vs ValidatorsByAddress) Len() int {
	return len(vs)
}

func (vs ValidatorsByAddress) Less(i, j int) bool {
	return bytes.Compare(vs[i].Address, vs[j].Address) == -1
}

func (vs ValidatorsByAddress) Swap(i, j int) {
	it := vs[i]
	vs[i] = vs[j]
	vs[j] = it
}

//-------------------------------------
// Use with Heap for sorting validators by accum

type accumComparable struct {
	*Validator
}

// We want to find the validator with the greatest accum.
func (ac accumComparable) Less(o interface{}) bool {
	other := o.(accumComparable).Validator
	larger := ac.CompareAccum(other)
	return bytes.Equal(larger.Address, ac.Address)
}

//----------------------------------------
// For testing

// NOTE: PrivValidator are in order.
func RandValidatorSet(numValidators int, votingPower int64) (*ValidatorSet, []*PrivValidator) {
	vals := make([]*Validator, numValidators)
	privValidators := make([]*PrivValidator, numValidators)
	for i := 0; i < numValidators; i++ {
		val, privValidator := RandValidator(false, votingPower)
		vals[i] = val
		privValidators[i] = privValidator
	}
	valSet := NewValidatorSet(vals)
	sort.Sort(PrivValidatorsByAddress(privValidators))
	return valSet, privValidators
}
