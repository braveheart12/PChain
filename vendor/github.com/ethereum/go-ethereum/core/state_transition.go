// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package core

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/vm"

	"github.com/ethereum/go-ethereum/params"
)

var (
	Big0 = big.NewInt(0)
)

/*
The State Transitioning Model

A state transition is a change made when a transaction is applied to the current world state
The state transitioning model does all all the necessary work to work out a valid new state root.

1) Nonce handling
2) Pre pay gas
3) Create a new state object if the recipient is \0*32
4) Value transfer
== If contract creation ==
  4a) Attempt to run transaction data
  4b) If valid, use result as code for the new state object
== end ==
5) Run Script section
6) Derive new state root
*/
type StateTransition struct {
	gp            *GasPool
	msg           Message
	gas, gasPrice *big.Int
	initialGas    *big.Int
	value         *big.Int
	data          []byte
	state         vm.StateDB

	env *vm.EVM
}

// Message represents a message sent to a contract.
type Message interface {
	From() common.Address
	//FromFrontier() (common.Address, error)
	To() *common.Address

	GasPrice() *big.Int
	Gas() *big.Int
	Value() *big.Int

	Nonce() uint64
	CheckNonce() bool
	Data() []byte
}

func MessageCreatesContract(msg Message) bool {
	return msg.To() == nil
}

// IntrinsicGas computes the 'intrinsic gas' for a message
// with the given data.
func IntrinsicGas(data []byte, contractCreation, homestead bool) *big.Int {
	igas := new(big.Int)
	if contractCreation && homestead {
		igas.Set(params.TxGasContractCreation)
	} else {
		igas.Set(params.TxGas)
	}
	logger.Infof("IntrinsicGas() 0, igas is %v, len(data) is %v\n", igas, len(data))
	if len(data) > 0 {
		var nz int64
		for _, byt := range data {
			if byt != 0 {
				nz++
			}
		}
		m := big.NewInt(nz)
		m.Mul(m, params.TxDataNonZeroGas)
		igas.Add(igas, m)
		logger.Infof("IntrinsicGas() 1, nz is %v, params.TxDataNonZeroGas is %v, igas is %v\n",
			nz, params.TxDataNonZeroGas, igas)
		m.SetInt64(int64(len(data)) - nz)
		m.Mul(m, params.TxDataZeroGas)
		igas.Add(igas, m)
		logger.Infof("IntrinsicGas() 2, len(data) - nz is %v, params.TxDataZeroGas is %v, igas is %v\n",
			int64(len(data))-nz, params.TxDataZeroGas, igas)
	}

	logger.Infof("IntrinsicGas() 3, igas is %v\n", igas)
	return igas
}

// NewStateTransition initialises and returns a new state transition object.
func NewStateTransition(env *vm.EVM, msg Message, gp *GasPool) *StateTransition {
	return &StateTransition{
		gp:         gp,
		env:        env,
		msg:        msg,
		gas:        new(big.Int),
		gasPrice:   msg.GasPrice(),
		initialGas: new(big.Int),
		value:      msg.Value(),
		data:       msg.Data(),
		state:      env.StateDB,
	}
}

// ApplyMessage computes the new state by applying the given message
// against the old state within the environment.
//
// ApplyMessage returns the bytes returned by any EVM execution (if it took place),
// the gas used (which includes gas refunds) and an error if it failed. An error always
// indicates a core error meaning that the message would always fail for that particular
// state and would never be accepted within a block.
func ApplyMessage(env *vm.EVM, msg Message, gp *GasPool) ([]byte, *big.Int, error) {
	st := NewStateTransition(env, msg, gp)

	ret, _, gasUsed, err := st.TransitionDb()
	logger.Infof("ApplyMessage() 0, gasUsed is %v\n", gasUsed)
	return ret, gasUsed, err
}

// ApplyMessage computes the new state by applying the given message
// against the old state within the environment.
//
// ApplyMessage returns the bytes returned by any EVM execution (if it took place),
// the gas used (which includes gas refunds) and an error if it failed. An error always
// indicates a core error meaning that the message would always fail for that particular
// state and would never be accepted within a block.
func ApplyMessageEx(env *vm.EVM, msg Message, gp *GasPool) ([]byte, *big.Int, *big.Int, error) {
	st := NewStateTransition(env, msg, gp)

	ret, _, gasUsed, moneyUsed, err := st.TransitionDbEx()
	logger.Infof("ApplyMessageEx(), gasUsed is %v, moneyUsed is %v\n", gasUsed, moneyUsed)
	return ret, gasUsed, moneyUsed, err
}

func (self *StateTransition) from() vm.Account {
	f := self.msg.From()
	if !self.state.Exist(f) {
		return self.state.CreateAccount(f)
	}
	return self.state.GetAccount(f)
}

func (self *StateTransition) to() vm.Account {
	if self.msg == nil {
		return nil
	}
	to := self.msg.To()
	if to == nil {
		return nil // contract creation
	}

	if !self.state.Exist(*to) {
		return self.state.CreateAccount(*to)
	}
	return self.state.GetAccount(*to)
}

func (self *StateTransition) useGas(amount *big.Int) error {

	logger.Infof("useGas() 0, self.gas is %v, amount is %v\n", self.gas, amount)
	if self.gas.Cmp(amount) < 0 {
		return vm.ErrOutOfGas
	}
	self.gas.Sub(self.gas, amount)
	logger.Infof("useGas() 1, self.gas is %v\n", self.gas)
	return nil
}

func (self *StateTransition) addGas(amount *big.Int) {
	self.gas.Add(self.gas, amount)
}

func (self *StateTransition) buyGas() error {
	mgas := self.msg.Gas()
	mgval := new(big.Int).Mul(mgas, self.gasPrice)

	sender := self.from()
	if sender.Balance().Cmp(mgval) < 0 {
		return fmt.Errorf("insufficient ETH for gas (%x). Req %v, has %v", sender.Address().Bytes()[:4], mgval, sender.Balance())
	}
	fmt.Printf("(self *StateTransition) buyGas(); sender is: %x, req: %v, has %v, gp is %v\n",
		sender.Address().Bytes(), mgval, sender.Balance(), self.gp)
	//debug.PrintStack()
	if err := self.gp.SubGas(mgas); err != nil {
		return err
	}
	self.addGas(mgas)
	self.initialGas.Set(mgas)
	sender.SubBalance(mgval)
	return nil
}

func (self *StateTransition) PreCheck() (err error) {
	msg := self.msg
	sender := self.from()

	// Make sure this transaction's nonce is correct
	if msg.CheckNonce() {
		if n := self.state.GetNonce(sender.Address()); n != msg.Nonce() {
			return NonceError(msg.Nonce(), n)
		}
	}

	// Pre-pay gas
	if err = self.buyGas(); err != nil {
		if IsGasLimitErr(err) {
			return err
		}
		return InvalidTxError(err)
	}

	return nil
}

// TransitionDb will move the state by applying the message against the given environment.
func (self *StateTransition) TransitionDb() (ret []byte, requiredGas, usedGas *big.Int, err error) {

	logger.Infof("TransitionDb(), before PreCheck\n")

	if err = self.PreCheck(); err != nil {
		return
	}

	//debug.PrintStack()

	msg := self.msg
	sender := self.from() // err checked in preCheck

	homestead := self.env.ChainConfig().IsHomestead(self.env.BlockNumber)
	contractCreation := MessageCreatesContract(msg)
	// Pay intrinsic gas
	logger.Infof("TransitionDb() 0, requiredGas is %v\n", requiredGas)
	if err = self.useGas(IntrinsicGas(self.data, contractCreation, homestead)); err != nil {
		return nil, nil, nil, InvalidTxError(err)
	}
	logger.Infof("TransitionDb() 1, self.gasUsed() is %v\n", self.gasUsed())
	var (
		vmenv = self.env
		// vm errors do not effect consensus and are therefor
		// not assigned to err, except for insufficient balance
		// error.
		vmerr error
	)

	if contractCreation {
		ret, _, vmerr = vmenv.Create(sender, self.data, self.gas, self.value)
		logger.Infof("TransitionDb() 2, self.gasUsed() is %v\n", self.gasUsed())
	} else {
		// Increment the nonce for the next transaction
		self.state.SetNonce(sender.Address(), self.state.GetNonce(sender.Address())+1)
		ret, vmerr = vmenv.Call(sender, self.to().Address(), self.data, self.gas, self.value)
	}
	if vmerr != nil {
		 logger.Error("vm returned with error:", err)
		// The only possible consensus-error would be if there wasn't
		// sufficient balance to make the transfer happen. The first
		// balance transfer may never fail.
		if vmerr == vm.ErrInsufficientBalance {
			return nil, nil, nil, InvalidTxError(vmerr)
		}
	}
	logger.Infof("TransitionDb() 3, self.gasUsed() is %v\n", self.gasUsed())
	requiredGas = new(big.Int).Set(self.gasUsed())
	logger.Infof("TransitionDb() 4, requiredGas is %v, self.gasUsed() is %v\n", requiredGas, self.gasUsed())

	self.refundGas()
	self.state.AddBalance(self.env.Coinbase, new(big.Int).Mul(self.gasUsed(), self.gasPrice))

	logger.Infof("TransitionDb() 5, requiredGas is %v, self.gasUsed() is %v\n", requiredGas, self.gasUsed())
	return ret, requiredGas, self.gasUsed(), err
}

// TransitionDbEx will move the state by applying the message against the given environment.
func (self *StateTransition) TransitionDbEx() (ret []byte, requiredGas, usedGas *big.Int, usedMoney *big.Int, err error) {

	logger.Infof("TransitionDbEx(), before PreCheck\n")

	if err = self.PreCheck(); err != nil {
		return
	}

	//debug.PrintStack()

	msg := self.msg
	sender := self.from() // err checked in preCheck

	homestead := self.env.ChainConfig().IsHomestead(self.env.BlockNumber)
	contractCreation := MessageCreatesContract(msg)
	// Pay intrinsic gas
	logger.Infof("TransitionDbEx() 0, requiredGas is %v\n", requiredGas)
	if err = self.useGas(IntrinsicGas(self.data, contractCreation, homestead)); err != nil {
		return nil, nil, nil, nil, InvalidTxError(err)
	}
	logger.Infof("TransitionDbEx() 1, self.gasUsed() is %v\n", self.gasUsed())
	var (
		vmenv = self.env
		// vm errors do not effect consensus and are therefor
		// not assigned to err, except for insufficient balance
		// error.
		vmerr error
	)

	if contractCreation {
		ret, _, vmerr = vmenv.Create(sender, self.data, self.gas, self.value)
		logger.Infof("TransitionDbEx() 2, self.gasUsed() is %v\n", self.gasUsed())
	} else {
		// Increment the nonce for the next transaction
		self.state.SetNonce(sender.Address(), self.state.GetNonce(sender.Address())+1)
		ret, vmerr = vmenv.Call(sender, self.to().Address(), self.data, self.gas, self.value)
	}
	if vmerr != nil {
		 logger.Error("vm returned with error:", err)
		// The only possible consensus-error would be if there wasn't
		// sufficient balance to make the transfer happen. The first
		// balance transfer may never fail.
		if vmerr == vm.ErrInsufficientBalance {
			return nil, nil, nil, nil, InvalidTxError(vmerr)
		}
	}
	logger.Infof("TransitionDbEx() 3, self.gasUsed() is %v\n", self.gasUsed())
	requiredGas = new(big.Int).Set(self.gasUsed())
	logger.Infof("TransitionDbEx() 4, requiredGas is %v, self.gasUsed() is %v\n", requiredGas, self.gasUsed())

	self.refundGas()
	//self.state.AddBalance(self.env.Coinbase, new(big.Int).Mul(self.gasUsed(), self.gasPrice))
	usedMoney = new(big.Int).Mul(self.gasUsed(), self.gasPrice)
	logger.Infof("TransitionDbEx() 5, requiredGas is %v, self.gasUsed() is %v, usedMoney is %v\n", requiredGas, self.gasUsed(), usedMoney)
	return ret, requiredGas, self.gasUsed(), usedMoney, err
}

func (self *StateTransition) refundGas() {
	// Return eth for remaining gas to the sender account,
	// exchanged at the original rate.
	sender := self.from() // err already checked
	remaining := new(big.Int).Mul(self.gas, self.gasPrice)
	sender.AddBalance(remaining)

	// Apply refund counter, capped to half of the used gas.
	uhalf := remaining.Div(self.gasUsed(), common.Big2)
	refund := common.BigMin(uhalf, self.state.GetRefund())
	self.gas.Add(self.gas, refund)
	self.state.AddBalance(sender.Address(), refund.Mul(refund, self.gasPrice))

	// Also return remaining gas to the block gas counter so it is
	// available for the next transaction.
	self.gp.AddGas(self.gas)
}

func (self *StateTransition) gasUsed() *big.Int {
	return new(big.Int).Sub(self.initialGas, self.gas)
}
