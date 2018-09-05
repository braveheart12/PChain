package core

import (
	"fmt"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"math/big"
)

// ApplyTransactionEx attempts to apply a transaction to the given state database
// and uses the input parameters for its environment. It returns the receipt
// for the transaction, gas used and an error if the transaction failed,
// indicating the block was invalid.
func ApplyTransactionEx(config *params.ChainConfig, bc *BlockChain, author *common.Address, gp *GasPool, statedb *state.StateDB,
	header *types.Header, tx *types.Transaction, usedGas *uint64, totalUsedMoney *big.Int, cfg vm.Config, cch CrossChainHelper) (*types.Receipt, uint64, error) {

	fmt.Printf("ApplyTransactionEx 0\n")

	msg, err := tx.AsMessage(types.MakeSigner(config, header.Number))
	if err != nil {
		return nil, 0, err
	}

	etd := tx.ExtendTxData()
	if etd == nil || etd.FuncName == "" {

		fmt.Printf("ApplyTransactionEx 1\n")

		// Create a new context to be used in the EVM environment
		context := NewEVMContext(msg, header, bc, author)

		fmt.Printf("ApplyTransactionEx 2\n")

		// Create a new environment which holds all relevant information
		// about the transaction and calling mechanisms.
		vmenv := vm.NewEVM(context, statedb, config, cfg)
		// Apply the transaction to the current state (included in the env)
		_, gas, money, failed, err := ApplyMessageEx(vmenv, msg, gp)
		if err != nil {
			return nil, 0, err
		}

		fmt.Printf("ApplyTransactionEx 3\n")
		// Update the state with pending changes
		var root []byte
		if config.IsByzantium(header.Number) {
			fmt.Printf("ApplyTransactionEx(), is byzantium\n")
			statedb.Finalise(true)
		} else {
			fmt.Printf("ApplyTransactionEx(), is not byzantium\n")
			root = statedb.IntermediateRoot(false).Bytes()
		}
		*usedGas += gas
		totalUsedMoney.Add(totalUsedMoney, money)

		// Create a new receipt for the transaction, storing the intermediate root and gas used by the tx
		// based on the eip phase, we're passing wether the root touch-delete accounts.
		receipt := types.NewReceipt(root, failed, *usedGas)
		fmt.Printf("ApplyTransactionEx，new receipt with (root,failed,*usedGas) = (%v,%v,%v)\n", root, failed, *usedGas)
		receipt.TxHash = tx.Hash()
		fmt.Printf("ApplyTransactionEx，new receipt with txhash %v\n", receipt.TxHash)
		receipt.GasUsed = gas
		fmt.Printf("ApplyTransactionEx，new receipt with gas %v\n", receipt.GasUsed)
		// if the transaction created a contract, store the creation address in the receipt.
		if msg.To() == nil {
			receipt.ContractAddress = crypto.CreateAddress(vmenv.Context.Origin, tx.Nonce())
		}
		// Set the receipt logs and create a bloom for filtering
		receipt.Logs = statedb.GetLogs(tx.Hash())
		fmt.Printf("ApplyTransactionEx，new receipt with receipt.Logs %v\n", receipt.Logs)
		receipt.Bloom = types.CreateBloom(types.Receipts{receipt})
		fmt.Printf("ApplyTransactionEx，new receipt with receipt.Bloom %v\n", receipt.Bloom)
		fmt.Printf("ApplyTransactionEx 4\n")
		return receipt, gas, err

	} else {

		logger.Infof("ApplyTransactionEx() 0, etd.FuncName is %v\n", etd.FuncName)

		// Pre-pay gas for extended tx.
		gas := tx.Gas()
		gasPrice := tx.GasPrice()
		gasValue := new(big.Int).Mul(new(big.Int).SetUint64(gas), gasPrice)
		from := msg.From()
		if statedb.GetBalance(from).Cmp(gasValue) < 0 {
			return nil, 0, fmt.Errorf("insufficient PAI for gas (%x). Req %v, has %v", from.Bytes()[:4], gasValue, statedb.GetBalance(from))
		}
		if err := gp.SubGas(gas); err != nil {
			return nil, 0, err
		}
		statedb.SubBalance(from, gasValue)
		logger.Infof("ApplyTransactionEx() 1, gas is %v, gasPrice is %v, gasValue is %v\n", gas, gasPrice, gasValue)

		if applyCb := GetApplyCb(etd.FuncName); applyCb != nil {
			cch.GetMutex().Lock()
			defer cch.GetMutex().Unlock()
			if err := applyCb(tx, statedb, cch); err != nil {
				return nil, 0, err
			}
		}

		*usedGas += gas
		totalUsedMoney.Add(totalUsedMoney, gasValue)
		logger.Infof("ApplyTransactionEx() 2, totalUsedMoney is %v\n", totalUsedMoney)

		// Update the state with pending changes
		var root []byte
		if config.IsByzantium(header.Number) {
			statedb.Finalise(true)
		} else {
			root = statedb.IntermediateRoot(config.IsEIP158(header.Number)).Bytes()
		}
		receipt := types.NewReceipt(root, true, *usedGas)
		receipt.TxHash = tx.Hash()
		receipt.GasUsed = gas

		// Set the receipt logs and create a bloom for filtering
		receipt.Logs = statedb.GetLogs(tx.Hash())
		receipt.Bloom = types.CreateBloom(types.Receipts{receipt})

		statedb.SetNonce(msg.From(), statedb.GetNonce(msg.From())+1)
		logger.Infof("ApplyTransactionEx() 3, totalUsedMoney is %v\n", totalUsedMoney)

		return receipt, 0, nil
	}
}
