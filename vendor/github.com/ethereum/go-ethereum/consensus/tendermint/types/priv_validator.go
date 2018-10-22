package types

import (
	"bytes"
	//"errors"
	"fmt"
	"io/ioutil"
	"os"
	"sync"

	crand "crypto/rand"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	ethcrypto "github.com/ethereum/go-ethereum/crypto"
	. "github.com/tendermint/go-common"
	"github.com/tendermint/go-crypto"
	"github.com/tendermint/go-wire"
	"bls"
	"github.com/ethereum/go-ethereum/common"
)

const (
	stepNone      = 0 // Used to distinguish the initial state
	stepPropose   = 1
	stepPrevote   = 2
	stepPrecommit = 3
)

func voteToStep(vote *Vote) int8 {
	switch vote.Type {
	case VoteTypePrevote:
		return stepPrevote
	case VoteTypePrecommit:
		return stepPrecommit
	default:
		PanicSanity("Unknown vote type")
		return 0
	}
}

type PrivValidator struct {
	Address []byte        `json:"address"`
	PubKey  crypto.PubKey `json:"pub_key"`

	// PrivKey should be empty if a Signer other than the default is being used.
	PrivKey crypto.PrivKey `json:"priv_key"`
	EthereumPubKey  crypto.PubKey `json:"ethereum_pub_key"`
	EthereumPrivKey crypto.PrivKey `json:"ethereum_priv_key"`
	EthereumAddress []byte         `json:"ethereum_address"`
	Signer  `json:"-"`

	// For persistence.
	// Overloaded for testing.
	filePath string
	mtx      sync.Mutex
}

// This is used to sign votes.
// It is the caller's duty to verify the msg before calling Sign,
// eg. to avoid double signing.
// Currently, the only callers are SignVote and SignProposal
type Signer interface {
	Sign(msg []byte) crypto.Signature
}

// Implements Signer
type DefaultSigner struct {
	priv crypto.PrivKey
}

func NewDefaultSigner(priv crypto.PrivKey) *DefaultSigner {
	return &DefaultSigner{priv: priv}
}

// Implements Signer
func (ds *DefaultSigner) Sign(msg []byte) crypto.Signature {
	return ds.priv.Sign(msg)
}

func (privVal *PrivValidator) SetSigner(s Signer) {
	privVal.Signer = s
}

func NewKeyStore(keydir string, scryptN, scryptP int) *keystore.KeyStorePassphrase {
	return keystore.NewKeyStoreByTenermint(keydir, scryptN, scryptP)
}

func GenPrivValidatorKey() (*PrivValidator, *keystore.Key) {
	newKey, err := keystore.NewKey(crand.Reader)
	if err != nil {
		return nil, nil
	}
	pubKey := crypto.EthereumPubKey(ethcrypto.FromECDSAPub(&(newKey.PrivateKey.PublicKey)))
	privKey := crypto.EthereumPrivKey(ethcrypto.FromECDSA(newKey.PrivateKey))


	keyPair := bls.GenerateKey()
	blsPrivKey := crypto.BLSPrivKey(keyPair.Private().Marshal())
	blsPubKey := blsPrivKey.PubKey()
	fmt.Println(common.ToHex(blsPrivKey.Bytes()))
	fmt.Println(common.ToHex(blsPrivKey.PubKey().Bytes()))
	msg := "hello world"
	sign := blsPrivKey.Sign([]byte(msg))
	fmt.Println("verify:", blsPrivKey.PubKey().VerifyBytes([]byte(msg), sign))
	fmt.Println(common.ToHex(blsPrivKey.PubKey().Bytes()))
	return &PrivValidator{
		Address:       blsPubKey.Address(),
		PubKey:        blsPubKey,
		PrivKey:       blsPrivKey,
		EthereumAddress:    pubKey.Address(),
		EthereumPrivKey:    privKey,
		EthereumPubKey:     pubKey,

		filePath: "",
		Signer:   NewDefaultSigner(blsPrivKey),
	}, newKey
}

// Generates a new validator with private key.
func GenPrivValidator(keydir string) *PrivValidator {
	scryptN := keystore.StandardScryptN
	scryptP := keystore.StandardScryptP
	//password := getPassPhrase("Your new account is locked with a password. Please give a password. Do not forget this password.", true)
	ks := keystore.NewKeyStoreByTenermint(keydir, scryptN, scryptP)
	newKey, err := keystore.NewKey(crand.Reader)
	if err != nil {
		return nil
	}
	a := accounts.Account{Address: newKey.Address, URL: accounts.URL{Scheme: keystore.KeyStoreScheme, Path: ks.Ks.JoinPath(keystore.KeyFileName(newKey.Address))}}
	if err := ks.StoreKey(a.URL.Path, newKey, "pchain"); err != nil {
		return nil
	}
	pubKey := crypto.EthereumPubKey(ethcrypto.FromECDSAPub(&(newKey.PrivateKey.PublicKey)))
	privKey := crypto.EthereumPrivKey(ethcrypto.FromECDSA(newKey.PrivateKey))


	keyPair := bls.GenerateKey()
	blsPrivKey := crypto.BLSPrivKey(keyPair.Private().Marshal())
	blsPubKey := blsPrivKey.PubKey()
	fmt.Println(common.ToHex(blsPrivKey.Bytes()))
	fmt.Println(common.ToHex(blsPrivKey.PubKey().Bytes()))
	msg := "hello world"
	sign := blsPrivKey.Sign([]byte(msg))
	fmt.Println("verify:", blsPrivKey.PubKey().VerifyBytes([]byte(msg), sign))
	fmt.Println(common.ToHex(blsPrivKey.PubKey().Bytes()))
	return &PrivValidator{
		Address:       blsPubKey.Address(),
		PubKey:        blsPubKey,
		PrivKey:       blsPrivKey,
		EthereumAddress:    pubKey.Address(),
		EthereumPrivKey:    privKey,
		EthereumPubKey:     pubKey,

		filePath: "",
		Signer:   NewDefaultSigner(blsPrivKey),
	}
}

func LoadPrivValidator(filePath string) *PrivValidator {
	privValJSONBytes, err := ioutil.ReadFile(filePath)
	if err != nil {
		Exit(err.Error())
	}
	privVal := wire.ReadJSON(&PrivValidator{}, privValJSONBytes, &err).(*PrivValidator)
	if err != nil {
		Exit(Fmt("Error reading PrivValidator from %v: %v\n", filePath, err))
	}
	privVal.filePath = filePath
	privVal.Signer = NewDefaultSigner(privVal.PrivKey)
	return privVal
}

func LoadOrGenPrivValidator(filePath, keydir string) *PrivValidator {
	var privValidator *PrivValidator
	if _, err := os.Stat(filePath); err == nil {
		privValidator = LoadPrivValidator(filePath)
		//logger.Infoln("Loaded PrivValidator", "file", filePath, "privValidator", privValidator)
	} else {
		privValidator = GenPrivValidator(keydir)
		privValidator.SetFile(filePath)
		privValidator.Save()
		//logger.Infoln("Generated PrivValidator", "file", filePath)
	}
	return privValidator
}

func (privVal *PrivValidator) SetFile(filePath string) {
	privVal.mtx.Lock()
	defer privVal.mtx.Unlock()
	privVal.filePath = filePath
}

func (privVal *PrivValidator) Save() {
	privVal.mtx.Lock()
	defer privVal.mtx.Unlock()
	privVal.save()
}

func (privVal *PrivValidator) save() {
	if privVal.filePath == "" {
		PanicSanity("Cannot save PrivValidator: filePath not set")
	}
	jsonBytes := wire.JSONBytesPretty(privVal)
	err := WriteFileAtomic(privVal.filePath, jsonBytes, 0600)
	if err != nil {
		// `@; BOOM!!!
		PanicCrisis(err)
	}
}

func (privVal *PrivValidator) GetAddress() []byte {
	return privVal.Address
}

func (priVal *PrivValidator) GetPubKey() crypto.PubKey {
	return priVal.PubKey
}

func (privVal *PrivValidator) SignVote(chainID string, vote *Vote) error {
	privVal.mtx.Lock()
	defer privVal.mtx.Unlock()

	signature := privVal.Sign(SignBytes(chainID, vote))
	vote.Signature = signature
	return nil
}

func (privVal *PrivValidator) SignProposal(chainID string, proposal *Proposal) error {
	privVal.mtx.Lock()
	defer privVal.mtx.Unlock()

	signature := privVal.Sign(SignBytes(chainID, proposal))
	proposal.Signature = signature
	return nil
}

func (privVal *PrivValidator) String() string {
	return fmt.Sprintf("PrivValidator{%X}", privVal.Address)
}

//-------------------------------------

type PrivValidatorsByAddress []*PrivValidator

func (pvs PrivValidatorsByAddress) Len() int {
	return len(pvs)
}

func (pvs PrivValidatorsByAddress) Less(i, j int) bool {
	return bytes.Compare(pvs[i].Address, pvs[j].Address) == -1
}

func (pvs PrivValidatorsByAddress) Swap(i, j int) {
	it := pvs[i]
	pvs[i] = pvs[j]
	pvs[j] = it
}
