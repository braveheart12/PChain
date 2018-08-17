package consensus

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/big"
	"path"
	"reflect"
	"strconv"
	"strings"
	"time"

	abci "github.com/tendermint/abci/types"
	auto "github.com/tendermint/go-autofile"
	. "github.com/tendermint/go-common"
	cfg "github.com/tendermint/go-config"
	"github.com/tendermint/go-wire"

	"github.com/tendermint/tendermint/proxy"
	rpcTxHook "github.com/tendermint/tendermint/rpc/core/txhook"
	sm "github.com/tendermint/tendermint/state"
	"github.com/tendermint/tendermint/types"
)

// Functionality to replay blocks and messages on recovery from a crash.
// There are two general failure scenarios: failure during consensus, and failure while applying the block.
// The former is handled by the WAL, the latter by the proxyApp Handshake on restart,
// which ultimately hands off the work to the WAL.

//-----------------------------------------
// recover from failure during consensus
// by replaying messages from the WAL

// Unmarshal and apply a single message to the consensus state
// as if it were received in receiveRoutine
// Lines that start with "#" are ignored.
// NOTE: receiveRoutine should not be running
func (cs *ConsensusState) readReplayMessage(msgBytes []byte, newStepCh chan interface{}) error {
	// Skip over empty and meta lines
	if len(msgBytes) == 0 || msgBytes[0] == '#' {
		return nil
	}
	var err error
	var msg TimedWALMessage
	wire.ReadJSON(&msg, msgBytes, &err)
	if err != nil {
		fmt.Println("MsgBytes:", msgBytes, string(msgBytes))
		return fmt.Errorf("Error reading json data: %v", err)
	}

	// for logging
	switch m := msg.Msg.(type) {
	case types.EventDataRoundState:
		logger.Info("Replay: New Step", " height:", m.Height, " round:", m.Round, " step:", m.Step)
		// these are playback checks
		ticker := time.After(time.Second * 2)
		if newStepCh != nil {
			select {
			case mi := <-newStepCh:
				m2 := mi.(types.EventDataRoundState)
				if m.Height != m2.Height || m.Round != m2.Round || m.Step != m2.Step {
					return fmt.Errorf("RoundState mismatch. Got %v; Expected %v", m2, m)
				}
			case <-ticker:
				return fmt.Errorf("Failed to read off newStepCh")
			}
		}
	case msgInfo:
		peerKey := m.PeerKey
		if peerKey == "" {
			peerKey = "local"
		}
		switch msg := m.Msg.(type) {
		case *ProposalMessage:
			p := msg.Proposal
			logger.Info("Replay: Proposal", " height:", p.Height, " round:", p.Round, " header:",
				p.BlockPartsHeader, " pol:", p.POLRound, " peer:", peerKey)
		case *BlockPartMessage:
			logger.Info("Replay: BlockPart", " height:", msg.Height, " round:", msg.Round, " peer:", peerKey)
		case *VoteMessage:
			v := msg.Vote
			logger.Info("Replay: Vote", " height:", v.Height, " round:", v.Round, " type:", v.Type,
				"blockID", v.BlockID, " peer:", peerKey)
		}

		cs.handleMsg(m, cs.RoundState)
	case timeoutInfo:
		logger.Info("Replay: Timeout", " height:", m.Height, " round:", m.Round, " step:", m.Step, " dur:", m.Duration)
		cs.handleTimeout(m, cs.RoundState)
	default:
		return fmt.Errorf("Replay: Unknown TimedWALMessage type: %v", reflect.TypeOf(msg.Msg))
	}
	return nil
}

// replay only those messages since the last block.
// timeoutRoutine should run concurrently to read off tickChan
func (cs *ConsensusState) catchupReplay(csHeight int) error {

	// set replayMode
	cs.replayMode = true
	defer func() { cs.replayMode = false }()

	// Ensure that ENDHEIGHT for this height doesn't exist
	// NOTE: This is just a sanity check. As far as we know things work fine without it,
	// and Handshake could reuse ConsensusState if it weren't for this check (since we can crash after writing ENDHEIGHT).
	gr, found, err := cs.wal.group.Search("#ENDHEIGHT: ", makeHeightSearchFunc(csHeight))
	if gr != nil {
		gr.Close()
	}
	if found {
		return errors.New(Fmt("WAL should not contain #ENDHEIGHT %d.", csHeight))
	}

	// Search for last height marker
	gr, found, err = cs.wal.group.Search("#ENDHEIGHT: ", makeHeightSearchFunc(csHeight-1))
	if err == io.EOF {
		logger.Warn("Replay: wal.group.Search returned EOF", " #ENDHEIGHT:", csHeight-1)
		// if we upgraded from 0.9 to 0.9.1, we may have #HEIGHT instead
		// TODO (0.10.0): remove this
		gr, found, err = cs.wal.group.Search("#HEIGHT: ", makeHeightSearchFunc(csHeight))
		if err == io.EOF {
			logger.Warn("Replay: wal.group.Search returned EOF", " #HEIGHT:", csHeight)
			return nil
		} else if err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		defer gr.Close()
	}
	if !found {
		// if we upgraded from 0.9 to 0.9.1, we may have #HEIGHT instead
		// TODO (0.10.0): remove this
		gr, found, err = cs.wal.group.Search("#HEIGHT: ", makeHeightSearchFunc(csHeight))
		if err == io.EOF {
			logger.Warn("Replay: wal.group.Search returned EOF", " #HEIGHT:", csHeight)
			return nil
		} else if err != nil {
			return err
		} else {
			defer gr.Close()
		}

		// TODO (0.10.0): uncomment
		// return errors.New(Fmt("Cannot replay height %d. WAL does not contain #ENDHEIGHT for %d.", csHeight, csHeight-1))
	}

	logger.Info("Catchup by replaying consensus messages", " height:", csHeight)

	for {
		line, err := gr.ReadLine()
		if err != nil {
			if err == io.EOF {
				break
			} else {
				return err
			}
		}
		// NOTE: since the priv key is set when the msgs are received
		// it will attempt to eg double sign but we can just ignore it
		// since the votes will be replayed and we'll get to the next step
		if err := cs.readReplayMessage([]byte(line), nil); err != nil {
			return err
		}
	}
	logger.Info("Replay: Done")
	return nil
}

//--------------------------------------------------------------------------------

// Parses marker lines of the form:
// #ENDHEIGHT: 12345
func makeHeightSearchFunc(height int) auto.SearchFunc {
	return func(line string) (int, error) {
		line = strings.TrimRight(line, "\n")
		parts := strings.Split(line, " ")
		if len(parts) != 2 {
			return -1, errors.New("Line did not have 2 parts")
		}
		i, err := strconv.Atoi(parts[1])
		if err != nil {
			return -1, errors.New("Failed to parse INFO: " + err.Error())
		}
		if height < i {
			return 1, nil
		} else if height == i {
			return 0, nil
		} else {
			return -1, nil
		}
	}
}

//----------------------------------------------
// Recover from failure during block processing
// by handshaking with the app to figure out where
// we were last and using the WAL to recover there

type Handshaker struct {
	config cfg.Config
	state  *sm.State
	store  types.BlockStore

	nBlocks int // number of blocks applied to the state

	cch rpcTxHook.CrossChainHelper
}

func NewHandshaker(config cfg.Config, state *sm.State, store types.BlockStore, cch rpcTxHook.CrossChainHelper) *Handshaker {
	return &Handshaker{config, state, store, 0, cch}
}

func (h *Handshaker) NBlocks() int {
	return h.nBlocks
}

var ErrReplayLastBlockTimeout = errors.New("Timed out waiting for last block to be replayed")

// TODO: retry the handshake/replay if it fails ?
func (h *Handshaker) Handshake(proxyApp proxy.AppConns) error {
	// handshake is done via info request on the query conn
	res, err := proxyApp.Query().InfoSync()
	if err != nil {
		return errors.New(Fmt("Error calling Info: %v", err))
	}

	blockHeight := int(res.LastBlockHeight) // XXX: beware overflow
	appHash := res.LastBlockAppHash

	logger.Info("ABCI Handshake", " appHeight:", blockHeight, " appHash:", appHash)

	// TODO: check version

	// replay blocks up to the latest in the blockstore
	_, err = h.ReplayBlocks(appHash, blockHeight, proxyApp, h.cch)
	if err == ErrReplayLastBlockTimeout {
		logger.Warn("Failed to sync via handshake. Trying other means. If they fail, please increase the timeout_handshake parameter")
		return nil

	} else if err != nil {
		return errors.New(Fmt("Error on replay: %v", err))
	}

	logger.Info("Completed ABCI Handshake - Tendermint and App are synced", " appHeight:", blockHeight, " appHash:", appHash)

	// TODO: (on restart) replay mempool

	return nil
}

//TODO: Be very careful, here may need to handle Epoch infomation
// Replay all blocks since appBlockHeight and ensure the result matches the current state.
// Returns the final AppHash or an error
func (h *Handshaker) ReplayBlocks(appHash []byte, appBlockHeight int, proxyApp proxy.AppConns, cch rpcTxHook.CrossChainHelper) ([]byte, error) {

	storeBlockHeight := h.store.Height()
	stateBlockHeight := h.state.LastBlockHeight
	logger.Info("ABCI Replay Blocks", " appHeight:", appBlockHeight, " storeHeight:", storeBlockHeight, " stateHeight:", stateBlockHeight)

	// First handle edge cases and constraints on the storeBlockHeight
	if storeBlockHeight == 0 {
		return appHash, h.checkAppHash(appHash)

	} else if storeBlockHeight < appBlockHeight {
		// the app should never be ahead of the store (but this is under app's control)
		return appHash, sm.ErrAppBlockHeightTooHigh{storeBlockHeight, appBlockHeight}

	} else if storeBlockHeight < stateBlockHeight {
		// the state should never be ahead of the store (this is under tendermint's control)
		PanicSanity(Fmt("StateBlockHeight (%d) > StoreBlockHeight (%d)", stateBlockHeight, storeBlockHeight))

	} else if storeBlockHeight > stateBlockHeight+1 {
		// store should be at most one ahead of the state (this is under tendermint's control)
		PanicSanity(Fmt("StoreBlockHeight (%d) > StateBlockHeight + 1 (%d)", storeBlockHeight, stateBlockHeight+1))
	}

	// Now either store is equal to state, or one ahead.
	// For each, consider all cases of where the app could be, given app <= store
	if storeBlockHeight == stateBlockHeight {
		// Tendermint ran Commit and saved the state.
		// Either the app is asking for replay, or we're all synced up.
		if appBlockHeight < storeBlockHeight {
			// the app is behind, so replay blocks, but no need to go through WAL (state is already synced to store)
			return h.replayBlocks(proxyApp, appBlockHeight, storeBlockHeight, false, cch)

		} else if appBlockHeight == storeBlockHeight {
			// We're good!
			return appHash, h.checkAppHash(appHash)
		}

	} else if storeBlockHeight == stateBlockHeight+1 {
		// We saved the block in the store but haven't updated the state,
		// so we'll need to replay a block using the WAL.
		if appBlockHeight < stateBlockHeight {
			// the app is further behind than it should be, so replay blocks
			// but leave the last block to go through the WAL
			return h.replayBlocks(proxyApp, appBlockHeight, storeBlockHeight, true, cch)

		} else if appBlockHeight == stateBlockHeight {
			// We haven't run Commit (both the state and app are one block behind),
			// so replayBlock with the real app.
			// NOTE: We could instead use the cs.WAL on cs.Start,
			// but we'd have to allow the WAL to replay a block that wrote it's ENDHEIGHT
			logger.Info("Replay last block using real app")
			return h.replayBlock(storeBlockHeight, proxyApp.Consensus(), cch)

		} else if appBlockHeight == storeBlockHeight {
			// We ran Commit, but didn't save the state, so replayBlock with mock app
			abciResponses, err := h.state.LoadABCIResponses(storeBlockHeight)
			if err != nil {
				return nil, err
			}
			mockApp := newMockProxyApp(appHash, abciResponses)
			logger.Info("Replay last block using mock app")
			return h.replayBlock(storeBlockHeight, mockApp, cch)
		}

	}

	PanicSanity("Should never happen")
	return nil, nil
}

//TODO: Be very careful, here may need to handle Epoch infomation
func (h *Handshaker) replayBlocks(proxyApp proxy.AppConns, appBlockHeight, storeBlockHeight int,
	mutateState bool, cch rpcTxHook.CrossChainHelper) ([]byte, error) {
	// App is further behind than it should be, so we need to replay blocks.
	// We replay all blocks from appBlockHeight+1.
	// Note that we don't have an old version of the state,
	// so we by-pass state validation/mutation using sm.ExecCommitBlock.
	// If mutateState == true, the final block is replayed with h.replayBlock()

	var appHash []byte
	var err error
	finalBlock := storeBlockHeight
	if mutateState {
		finalBlock -= 1
	}
	for i := appBlockHeight + 1; i <= finalBlock; i++ {
		logger.Info("Applying block", " height:", i)
		block := h.store.LoadBlock(i)

		appHash, err = sm.ExecCommitBlock(proxyApp.Consensus(), h.state, block, cch)
		if err != nil {
			return nil, err
		}

		h.nBlocks += 1
	}

	if mutateState {
		// sync the final block
		return h.replayBlock(storeBlockHeight, proxyApp.Consensus(), cch)
	}

	return appHash, h.checkAppHash(appHash)
}

// ApplyBlock on the proxyApp with the last block.
func (h *Handshaker) replayBlock(height int, proxyApp proxy.AppConnConsensus, cch rpcTxHook.CrossChainHelper) ([]byte, error) {
	mempool := types.MockMempool{}

	var eventCache types.Fireable // nil
	block := h.store.LoadBlock(height)
	meta := h.store.LoadBlockMeta(height)

	if err := h.state.ApplyBlock(eventCache, proxyApp, block, meta.BlockID.PartsHeader, mempool, cch); err != nil {
		return nil, err
	}

	h.nBlocks += 1

	return h.state.AppHash, nil
}

func (h *Handshaker) checkAppHash(appHash []byte) error {
	if !bytes.Equal(h.state.AppHash, appHash) {
		panic(errors.New(Fmt("Tendermint state.AppHash does not match AppHash after replay. Got %X, expected %X", appHash, h.state.AppHash)).Error())
		return nil
	}
	return nil
}

//--------------------------------------------------------------------------------
// mockProxyApp uses ABCIResponses to give the right results
// Useful because we don't want to call Commit() twice for the same block on the real app.

func newMockProxyApp(appHash []byte, abciResponses *sm.ABCIResponses) proxy.AppConnConsensus {
	clientCreator := proxy.NewLocalClientCreator(&mockProxyApp{
		appHash:       appHash,
		abciResponses: abciResponses,
	})
	cli, _ := clientCreator.NewABCIClient()
	return proxy.NewAppConnConsensus(cli)
}

type mockProxyApp struct {
	abci.BaseApplication

	appHash       []byte
	txCount       int
	abciResponses *sm.ABCIResponses
}

func (mock *mockProxyApp) DeliverTx(tx []byte) abci.Result {
	r := mock.abciResponses.DeliverTx[mock.txCount]
	mock.txCount += 1
	return abci.Result{
		r.Code,
		r.Data,
		r.Log,
	}
}

func (mock *mockProxyApp) EndBlock(height uint64) abci.ResponseEndBlock {
	mock.txCount = 0
	return mock.abciResponses.EndBlock
}

func (mock *mockProxyApp) Commit(validators []*abci.Validator, rewardPerBlock *big.Int, refund []*abci.RefundValidatorAmount) abci.Result {
	return abci.NewResultOK(mock.appHash, "")
}

//-------------------------------------
//liaoyd
// Open file to log all validator change and timeouts for deterministic accountability
func OpenVAL(valFile string) (val *VAL, err error) {
	err = EnsureDir(path.Dir(valFile), 0700)
	if err != nil {
		logger.Error("Error ensuring ConsensusState val dir", "error", err.Error())
		return nil, err
	}

	val, err = NewVAL(valFile)
	LoadChangedVals(val) //load all changed to map
	if err != nil {
		return val, err
	}
	return val, nil
}

func CatchupValidator(val *VAL, height int, preVals *types.ValidatorSet) error {
	fmt.Println("in func (conR *ConsensusReactor) catchupValidator(val *VAL) error")
	//TODO
	// height := conR.conS.Height
	fmt.Println("height:", height)
	// gr, found, err := val.group.SearchMaxLower("#HEIGHT: ", auto.MakeSimpleSearchFunc("#HEIGHT: ", height))
	gr, _, _ := val.group.SearchMaxLower("#HEIGHT: ", makeHeightSearchFunc(height))

	if gr == nil {
		fmt.Println("gr is nil!!!!!!!!!")
		return nil
	}
	// if gr.curIndex == 0 { //don't need to update validator
	// 	return nil
	// }
	for { //TODO update validator
		line, err := gr.ReadLine()
		if err != nil {
			if err == io.EOF { //no new line don't need to update validator
				break
			} else {
				return err
			}
		}
		if err = readReplayValMessage([]byte(line), preVals); err != nil {
			return err
		}
		fmt.Println("line:", line)
	}
	return nil
}

func readReplayValMessage(msgBytes []byte, preVals *types.ValidatorSet) error {
	fmt.Println("func (conR *ConsensusReactor) readReplayMessage(msgBytes []byte, newStepCh chan interface{}) error")
	// Skip over empty and meta lines
	if len(msgBytes) == 0 || msgBytes[0] == '#' { //TODO
		return nil
	}
	var err error
	var msg TimedVALMessage
	wire.ReadJSON(&msg, msgBytes, &err)
	if err != nil {
		fmt.Println("MsgBytes:", msgBytes, string(msgBytes))
		return fmt.Errorf("Error reading json data: %v", err)
	}
	// fmt.Println("msg.Msg:", msg.Msg)
	// for logging
	switch m := msg.Msg.(type) {
	case *types.PreVal:
		// fmt.Println("PreVal")
		var vals []*abci.Validator
		for _, val := range m.ValidatorSet.Validators {
			// fmt.Println("val:", val)
			vals = append(
				vals,
				&abci.Validator{
					PubKey: val.PubKey.Bytes(),
					Power:  val.VotingPower,
				},
			)
		}
		// fmt.Println("vals:", vals)
		types.UpdateValidators(preVals, vals)
		// types.DurStart <- vals
		//TODO update validators
		// <-types.EndStart //waiting for update end
		return nil
	case *types.AcceptVotes:
		// fmt.Println("AcceptVotes")
		var vals []*abci.Validator
		vals = append(
			vals,
			&abci.Validator{
				PubKey: m.PubKey.Bytes(),
				Power:  m.Power,
			},
		)
		// fmt.Println("vals:", vals)
		types.UpdateValidators(preVals, vals)
		// types.DurStart <- vals
		// <-types.EndStart
		return nil
	default:
		fmt.Println("default")
		return fmt.Errorf("Replay: Unknown TimedVALMessage type: %v", reflect.TypeOf(msg.Msg))
	}
	return nil
}

func LoadChangedVals(val *VAL) error {
	types.ValChangedEpoch = make(map[int][]*types.AcceptVotes)
	// gr := valFile.Group.NewGroupReader(val.group)
	gr := auto.NewGroupReader(val.group)
	// prefix := "#HEIGHT: "
	// var msg TimedVALMessage
	for {
		line, err := gr.ReadLine()
		if err != nil {
			if err == io.EOF { //no new line don't need to update validator
				break
			} else {
				return err
			}
		}
		StoreChangedValsToMap([]byte(line))
	}
	return nil
}

func StoreChangedValsToMap(msgBytes []byte) error {
	fmt.Println("in func StoreChangedValsToMap(msgBytes []byte) error")
	var err error
	var msg TimedVALMessage
	changed := types.ValChangedEpoch
	if msgBytes[0] == '#' {
		return nil
		// height, _ := strconv.Atoi(string(msgBytes[9:]))
		// if _, ok := changed[height]; ok {
		// 	fmt.Println("val height error!")
		// 	return errors.New("val height error!")
		// }
		// fmt.Println("make changed vals array at height:", height)
		// changed[height] = make([]*types.AcceptVotes) //dont need to init slice
	}
	wire.ReadJSON(&msg, msgBytes, &err)
	switch m := msg.Msg.(type) {
	case *types.PreVal:
		fmt.Println("preval break")
		break
	case *types.AcceptVotes:
		fmt.Println("acceptvotes store")
		if _, ok := changed[m.Epoch]; ok {
			changed[m.Epoch] = append(
				changed[m.Epoch],
				m,
			)
			fmt.Println("m:", m)
			return nil
		} else {
			fmt.Println("accept votes error")
			return errors.New("accept votes error")
		}
	default:
		fmt.Println("unknown type")
		return errors.New("unknown type")
	}
	return nil
}
