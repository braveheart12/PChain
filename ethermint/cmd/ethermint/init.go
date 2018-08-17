package etmmain

import (
	"os"
	"path/filepath"

	"gopkg.in/urfave/cli.v1"

	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/ethdb"

	"encoding/json"
	"fmt"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/console"
	"github.com/pkg/errors"
	cmn "github.com/tendermint/go-common"
	"github.com/tendermint/go-crypto"
	"github.com/tendermint/tendermint/types"
	"io/ioutil"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type BalaceAmount struct {
	balance string
	amount  string
}

type InvalidArgs struct {
	args string
}

func (invalid InvalidArgs) Error() string {
	return "invalid args:" + invalid.args
}

func parseBalaceAmount(s string) ([]*BalaceAmount, error) {
	r, _ := regexp.Compile("\\{[\\ \\t]*\\d+(\\.\\d+)?[\\ \\t]*\\,[\\ \\t]*\\d+(\\.\\d+)?[\\ \\t]*\\}")
	parse_strs := r.FindAllString(s, -1)
	if len(parse_strs) == 0 {
		return nil, InvalidArgs{s}
	}
	balanceAmounts := make([]*BalaceAmount, len(parse_strs))
	for i, v := range parse_strs {
		length := len(v)
		balanceAmount := strings.Split(v[1:length-1], ",")
		if len(balanceAmount) != 2 {
			return nil, InvalidArgs{s}
		}
		balanceAmounts[i] = &BalaceAmount{strings.TrimSpace(balanceAmount[0]), strings.TrimSpace(balanceAmount[1])}
	}
	return balanceAmounts, nil
}

func initCmd(ctx *cli.Context) error {

	// ethereum genesis.json
	ethGenesisPath := ctx.Args().First()
	if len(ethGenesisPath) == 0 {
		utils.Fatalf("must supply path to genesis JSON file")
	}
	//coreGensis, privValidator, err := init_eth_genesis()
	//if err != nil {
	//	utils.Fatalf("init eth_genesis failed")
	//	return err
	//}
	init_eth_blockchain(config.GetString("eth_genesis_file"), ctx)

	init_em_files(ethGenesisPath)
	/*
		rsTmplPath := ctx.Args().Get(1)
		init_reward_scheme_files(rsTmplPath)
	*/
	return nil
}

func initEthGenesis(ctx *cli.Context) error {
	fmt.Println("this is init_eth_genesis")
	args := ctx.Args()
	if len(args) != 1 {
		utils.Fatalf("len of args is %d", len(args))
		return nil
	}
	bal_str := args[0]
	balanceAmounts, err := parseBalaceAmount(bal_str)
	if err != nil {
		utils.Fatalf("init eth_genesis_file failed")
		return err
	}

	validators := createPriValidators(len(balanceAmounts))

	var coreGenesis = core.Genesis{
		Nonce:      "0xdeadbeefdeadbeef",
		Timestamp:  "0x0",
		ParentHash: "0x0000000000000000000000000000000000000000000000000000000000000000",
		ExtraData:  "0x0",
		GasLimit:   "0x8000000",
		Difficulty: "0x400",
		Mixhash:    "0x0000000000000000000000000000000000000000000000000000000000000000",
		Coinbase:   common.ToHex((*validators[0]).Address),
		Alloc: map[string]struct {
			Code    string
			Storage map[string]string
			Balance string
			Nonce   string
			Amount  string
			PubKey  string
			ConsensusPubKey string
		}{},
	}
	for i, validator := range validators {
		otherConPub, l := validator.PubKey.(crypto.BLSPubKey)
		otherEthPub, r := validator.EthereumPubKey.(crypto.EthereumPubKey)
		if l&&r {
			coreGenesis.Alloc[common.ToHex(validator.EthereumPubKey.Address())] = struct {
				Code    string
				Storage map[string]string
				Balance string
				Nonce   string
				Amount  string
				PubKey  string
				ConsensusPubKey string
			}{Balance: balanceAmounts[i].balance, Amount:balanceAmounts[i].amount, PubKey: common.ToHex(otherEthPub[:]), ConsensusPubKey:common.ToHex(otherConPub[:])}
		}
	}

	contents, err := json.Marshal(coreGenesis)
	if err != nil {
		utils.Fatalf("marshal coreGenesis failed")
		return err
	}

	var object interface{}
	err = json.Unmarshal(contents, &object)
	if err != nil {
		utils.Fatalf("write eth_genesis_file failed")
	}
	contents, err = json.MarshalIndent(object, "", "\t")

	if err != nil {
		utils.Fatalf("write eth_genesis_file failed")
	}

	ethGenesisPath := config.GetString("eth_genesis_file")
	if err = ioutil.WriteFile(ethGenesisPath, contents, 0654); err != nil {
		utils.Fatalf("write eth_genesis_file failed")
		return err
	}
	return nil
}

func init_eth_blockchain(ethGenesisPath string, ctx *cli.Context) {

	chainDb, err := ethdb.NewLDBDatabase(filepath.Join(utils.MakeDataDir(ctx), "chaindata"), 0, 0)
	if err != nil {
		utils.Fatalf("could not open database: %v", err)
	}

	genesisFile, err := os.Open(ethGenesisPath)
	if err != nil {
		utils.Fatalf("failed to read genesis file: %v", err)
	}

	block, err := core.WriteGenesisBlock(chainDb, genesisFile)
	if err != nil {
		utils.Fatalf("failed to write genesis block: %v", err)
	}

	logger.Infof("successfully wrote genesis block and/or chain rule set: %x", block.Hash())
}

func init_em_files(genesisPath string) error {
	gensisFile, err := os.Open(genesisPath)
	defer gensisFile.Close()
	if err != nil {
		utils.Fatalf("failed to read Ethereume genesis file: %v", err)
		return err
	}
	contents, err := ioutil.ReadAll(gensisFile)
	if err != nil {
		utils.Fatalf("failed to read Ethereume genesis file: %v", err)
		return err
	}
	var coreGenesis = core.Genesis{}
	if err := json.Unmarshal(contents, &coreGenesis); err != nil {
		return err
	}
	privValPath := config.GetString("priv_validator_file")
	keydir := config.GetString("keystore")
	if _, err := os.Stat(privValPath); os.IsNotExist(err) {
		utils.Fatalf("failed to read privValidator file: %v", err)
		return err
	}
	privValidator := types.LoadOrGenPrivValidator(privValPath, keydir)
	if err := createGenesisDoc(&coreGenesis, privValidator); err != nil {
		utils.Fatalf("failed to write genesis file: %v", err)
		return err
	}
	return nil
}

func createGenesisDoc(coreGenesis *core.Genesis, privValidator *types.PrivValidator) error {
	genFile := config.GetString("genesis_file")
	if _, err := os.Stat(genFile); os.IsNotExist(err) {
		genDoc := types.GenesisDoc{
			ChainID:   cmn.Fmt("test-chain-%v", cmn.RandStr(6)),
			Consensus: types.CONSENSUS_POS,
			RewardScheme: types.RewardSchemeDoc{
				TotalReward:        "210000000000000000000000000",
				PreAllocated:       "178500000000000000000000000",
				AddedPerYear:       "0",
				RewardFirstYear:    "5727300000000000000000000",
				DescendPerYear:     "572730000000000000000000",
				Allocated:          "0",
				EpochNumberPerYear: "12",
			},
			CurrentEpoch: types.OneEpochDoc{
				Number:         "0",
				RewardPerBlock: "184133873456790100",
				StartBlock:     "0",
				EndBlock:       "2592000",
				StartTime:      time.Now().Format(time.RFC3339Nano),
				EndTime:        "0", //not accurate for current epoch
				BlockGenerated: "0",
				Status:         "0",
			},
		}

		coinbase, amount, checkErr := checkAccount(*coreGenesis)
		if checkErr != nil {
			logger.Infof(checkErr.Error())
			cmn.Exit(checkErr.Error())
		}

		genDoc.CurrentEpoch.Validators = []types.GenesisValidator{types.GenesisValidator{
			EthAccount: coinbase,
			PubKey:     privValidator.PubKey,
			Amount:     amount,
		}}
		genDoc.SaveAs(genFile)
	}
	return nil
}

func createPriValidators(num int) []*types.PrivValidator {
	validators := make([]*types.PrivValidator, num)
	var newKey *keystore.Key
	scryptN := keystore.StandardScryptN
	scryptP := keystore.StandardScryptP
	ks := keystore.NewKeyStoreByTenermint(config.GetString("keystore"), scryptN, scryptP)
	privValFile := config.GetString("priv_validator_file_root")
	for i := 0; i < num; i++ {
		validators[i], newKey = types.GenPrivValidatorKey()
		privKey := validators[i].EthereumPrivKey.(crypto.EthereumPrivKey)
		pwd := common.ToHex(privKey[0:7])
		pwd = string([]byte(pwd)[2:])
		pwd = strings.ToUpper(pwd)
		pwd = "pchain"
		fmt.Println("account:",common.ToHex(validators[i].Address), "pwd:", pwd)
		a := accounts.Account{Address: newKey.Address, URL: accounts.URL{Scheme: keystore.KeyStoreScheme, Path: ks.Ks.JoinPath(keystore.KeyFileName(newKey.Address))}}
		if err := ks.StoreKey(a.URL.Path, newKey, pwd); err != nil {
			utils.Fatalf("store key failed")
			return nil
		}
		if i > 0 {
			validators[i].SetFile(privValFile + strconv.Itoa(i) + ".json")
		} else {
			validators[i].SetFile(privValFile + ".json")
		}
		validators[i].Save()
	}
	return validators
}

func checkAccount(coreGenesis core.Genesis) (common.Address, *big.Int, error) {

	coinbase := common.HexToAddress(coreGenesis.Coinbase)
	amount := big.NewInt(0)
	balance := big.NewInt(-1)
	found := false
	fmt.Printf("checkAccount(), coinbase is %x\n", coinbase)
	for addr, account := range coreGenesis.Alloc {
		address := common.HexToAddress(addr)
		fmt.Printf("checkAccount(), address is %x\n", address)
		if coinbase == address {
			balance = common.String2Big(account.Balance)
			amount = common.String2Big(account.Amount)
			found = true
			break
		}
	}
	if !found {
		fmt.Printf("invalidate eth_account\n")
		return common.Address{}, amount, errors.New("invalidate eth_account")
	}

	if balance.Cmp(amount) < 0 {
		fmt.Printf("balance is not enough to be support validator's amount, balance is %v, amount is %v\n",
			balance, amount)
		return common.Address{}, amount, errors.New("no enough balance")
	}
	return coinbase, amount, nil
}

// getPassPhrase retrieves the passwor associated with an account, either fetched
// from a list of preloaded passphrases, or requested interactively from the user.
func getPassPhrase(prompt string, confirmation bool) string {
	//prompt the user for the password
	if prompt != "" {
		fmt.Println(prompt)
	}
	password, err := console.Stdin.PromptPassword("Passphrase: ")
	if err != nil {
		utils.Fatalf("Failed to read passphrase: %v", err)
	}
	if confirmation {
		confirm, err := console.Stdin.PromptPassword("Repeat passphrase: ")
		if err != nil {
			utils.Fatalf("Failed to read passphrase confirmation: %v", err)
		}
		if password != confirm {
			utils.Fatalf("Passphrases do not match")
		}
	}
	return password
}
