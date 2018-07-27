package types

import (
	"bytes"
	"fmt"
	"io"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	abciTypes "github.com/tendermint/abci/types"
	. "github.com/tendermint/go-common"
	"github.com/tendermint/go-crypto"
	"github.com/tendermint/go-wire"
)

// Volatile state for each Validator
// TODO: make non-volatile identity
// 	- Remove Accum - it can be computed, and now valset becomes identifying
type Validator struct {
	Address     []byte        `json:"address"`
	PubKey      crypto.PubKey `json:"pub_key"`
	VotingPower *big.Int      `json:"voting_power"`
	Accum       *big.Int      `json:"accum"`
}

func NewValidator(pubKey crypto.PubKey, votingPower *big.Int) *Validator {
	return &Validator{
		Address:     pubKey.Address(),
		PubKey:      pubKey,
		VotingPower: votingPower,
		Accum:       big.NewInt(0),
	}
}

// Creates a new copy of the validator so we can mutate accum.
// Panics if the validator is nil.
func (v *Validator) Copy() *Validator {
	vCopy := *v
	vCopy.VotingPower = new(big.Int).Set(v.VotingPower)
	vCopy.Accum = new(big.Int).Set(v.Accum)
	return &vCopy
}

func (v *Validator) Equals(other *Validator) bool {

	return bytes.Equal(v.Address, other.Address) &&
		v.PubKey.Equals(other.PubKey) &&
		v.VotingPower.Cmp(other.VotingPower) == 0
}

// Returns the one with higher Accum.
func (v *Validator) CompareAccum(other *Validator) *Validator {
	if v == nil {
		return other
	}
	if v.Accum.Cmp(other.Accum) == 1 {
		return v
	} else if v.Accum.Cmp(other.Accum) == -1 {
		return other
	} else {
		if bytes.Compare(v.Address, other.Address) < 0 {
			return v
		} else if bytes.Compare(v.Address, other.Address) > 0 {
			return other
		} else {
			PanicSanity("Cannot compare identical validators")
			return nil
		}
	}
}

func (v *Validator) String() string {
	if v == nil {
		return "nil-Validator"
	}
	return fmt.Sprintf("Validator{%X %v VP:%v A:%v}",
		v.Address,
		v.PubKey,
		v.VotingPower,
		v.Accum)
}

func (v *Validator) Hash() []byte {
	return wire.BinaryRipemd160(v)
}

func (v *Validator) ToAbciValidator() *abciTypes.Validator {
	return &abciTypes.Validator{
		Address: common.BytesToAddress(v.Address),
		PubKey:  v.PubKey.Bytes(),
		Power:   v.VotingPower,
	}
}

//-------------------------------------

var ValidatorCodec = validatorCodec{}

type validatorCodec struct{}

func (vc validatorCodec) Encode(o interface{}, w io.Writer, n *int, err *error) {
	wire.WriteBinary(o.(*Validator), w, n, err)
}

func (vc validatorCodec) Decode(r io.Reader, n *int, err *error) interface{} {
	return wire.ReadBinary(&Validator{}, r, 0, n, err)
}

func (vc validatorCodec) Compare(o1 interface{}, o2 interface{}) int {
	PanicSanity("ValidatorCodec.Compare not implemented")
	return 0
}

//--------------------------------------------------------------------------------
// For testing...

func RandValidator(randPower bool, minPower int64) (*Validator, *PrivValidator) {
	privVal := GenPrivValidator()
	_, tempFilePath := Tempfile("priv_validator_")
	privVal.SetFile(tempFilePath)
	votePower := minPower
	if randPower {
		votePower += int64(RandUint32())
	}
	val := NewValidator(privVal.PubKey, big.NewInt(votePower))
	return val, privVal
}
