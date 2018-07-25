package blockchain

import (
	"bytes"
	"errors"
	"reflect"
	"time"

	cmn "github.com/tendermint/go-common"
	cfg "github.com/tendermint/go-config"
	"github.com/tendermint/go-p2p"
	"github.com/tendermint/go-wire"
	"github.com/tendermint/tendermint/proxy"
	sm "github.com/tendermint/tendermint/state"
	"github.com/tendermint/tendermint/types"
	"fmt"
)

const (
	// BlockchainChannel is a channel for blocks and status updates (`BlockStore` height)
	BlockchainChannel = byte(0x40)

	defaultChannelCapacity = 100
	defaultSleepIntervalMS = 500
	trySyncIntervalMS      = 100
	// stop syncing when last block's time is
	// within this much of the system time.
	// stopSyncingDurationMinutes = 10

	// ask for best height every 10s
	statusUpdateIntervalSeconds = 10
	// check if we should switch to consensus reactor
	switchToConsensusIntervalSeconds = 1
	maxBlockchainResponseSize        = types.MaxBlockSize + 2
)

type consensusReactor interface {
	// for when we switch from blockchain reactor and fast sync to
	// the consensus machine
	SwitchToConsensus(*sm.State)
}

// BlockchainReactor handles long-term catchup syncing.
type BlockchainReactor struct {
	p2p.BaseReactor

	config       cfg.Config
	state        *sm.State
	proxyAppConn proxy.AppConnConsensus // same as consensus.proxyAppConn
	store        *BlockStore
	pool         *BlockPool
	fastSync     bool
	requestsCh   chan BlockRequest
	timeoutsCh   chan string
	lastBlock    *types.Block

	evsw types.EventSwitch
}

// NewBlockchainReactor returns new reactor instance.
func NewBlockchainReactor(config cfg.Config, state *sm.State, proxyAppConn proxy.AppConnConsensus, store *BlockStore, fastSync bool) *BlockchainReactor {
	if state.LastBlockHeight == store.Height()-1 {
		store.height-- // XXX HACK, make this better
	}
	if state.LastBlockHeight != store.Height() {
		cmn.PanicSanity(cmn.Fmt("state (%v) and store (%v) height mismatch", state.LastBlockHeight, store.Height()))
	}
	requestsCh := make(chan BlockRequest, defaultChannelCapacity)
	timeoutsCh := make(chan string, defaultChannelCapacity)
	pool := NewBlockPool(
		store.Height()+1,
		requestsCh,
		timeoutsCh,
	)
	bcR := &BlockchainReactor{
		config:       config,
		state:        state,
		proxyAppConn: proxyAppConn,
		store:        store,
		pool:         pool,
		fastSync:     fastSync,
		requestsCh:   requestsCh,
		timeoutsCh:   timeoutsCh,
	}
	bcR.BaseReactor = *p2p.NewBaseReactor(logger, "BlockchainReactor", bcR)
	return bcR
}

// OnStart implements BaseService
func (bcR *BlockchainReactor) OnStart() error {
	bcR.BaseReactor.OnStart()
	conR := bcR.Switch.Reactor("CONSENSUS").(consensusReactor)
	conR.SwitchToConsensus(bcR.state)
	/*if bcR.fastSync {
		_, err := bcR.pool.Start()
		if err != nil {
			return err
		}
		go bcR.poolRoutine()
	}*/
	return nil
}

func (bcR *BlockchainReactor) ReStartPool() error {
	bcR.BaseReactor.IsRunning()
	bcR.pool.IsRunning()
	if bcR.fastSync {
		pool := NewBlockPool(
			bcR.store.Height()+1,
			bcR.requestsCh,
			bcR.timeoutsCh,
		)
		bcR.pool = pool
		_, err := bcR.pool.Start()
		if err != nil {
			return err
		}
		go bcR.BroadcastStatusRequest()
		go bcR.poolRoutine()
	}
	return nil
}

// OnStop implements BaseService
func (bcR *BlockchainReactor) OnStop() {
	bcR.BaseReactor.OnStop()
	bcR.pool.Stop()
}

// GetChannels implements Reactor
func (bcR *BlockchainReactor) GetChannels() []*p2p.ChannelDescriptor {
	return []*p2p.ChannelDescriptor{
		&p2p.ChannelDescriptor{
			ID:                BlockchainChannel,
			Priority:          5,
			SendQueueCapacity: 100,
		},
	}
}

// AddPeer implements Reactor by sending our state to peer.
func (bcR *BlockchainReactor) AddPeer(peer *p2p.Peer) {
	if !peer.Send(BlockchainChannel, struct{ BlockchainMessage }{&bcStatusResponseMessage{bcR.store.Height()}}) {
		// doing nothing, will try later in `poolRoutine`
	}
}

// RemovePeer implements Reactor by removing peer from the pool.
func (bcR *BlockchainReactor) RemovePeer(peer *p2p.Peer, reason interface{}) {
	bcR.pool.RemovePeer(peer.Key)
}

// Receive implements Reactor by handling 4 types of messages (look below).
func (bcR *BlockchainReactor) Receive(chID byte, src *p2p.Peer, msgBytes []byte) {
	_, msg, err := DecodeMessage(msgBytes)
	if err != nil {
		logger.Warn("Error decoding message", " error:", err)
		return
	}

	logger.Debug("Receive", " src:", src, " chID:", chID, " msg:", msg)

	var NON_ACCOUNT string = ""
	var NON_EPOCH int = -1

	switch msg := msg.(type) {
	case *bcBlockRequestMessage:
		// Got a request for a block. Respond with block if we have it.
		block := bcR.store.LoadBlock(msg.Height)
		if block != nil {
			msg := &bcBlockResponseMessage{Block: block}
			queued := src.TrySend(BlockchainChannel, struct{ BlockchainMessage }{msg})
			if !queued {
				// queue is full, just ignore.
			}
		} else {
			// TODO peer is asking for things we don't have.
		}
	case *bcBlockResponseMessage:
		// Got a block.
		bcR.pool.AddBlock(src.Key, msg.Block, len(msgBytes))
	case *bcStatusRequestMessage:
		// Send peer our state.
		queued := src.TrySend(BlockchainChannel, struct{ BlockchainMessage }{&bcStatusResponseMessage{bcR.store.Height()}})
		if !queued {
			// sorry
		}
	case *bcStatusResponseMessage:
		// Got a peer status. Unverified.
		bcR.pool.SetPeerHeight(src.Key, msg.Height)
	case *bcValidatorRequestMessage:

		for epoch, v := range types.ValChangedEpoch {
			for i:=0; i<len(v); i++ {
				valMsg := &bcValidatorResponseMessage{Account: NON_ACCOUNT, Epoch: epoch, AcceptVotes: v[i]}
				fmt.Println("sending bcValidatorResponseMessage")
				fmt.Println("valMsg:", valMsg)
				queued := src.TrySend(BlockchainChannel, struct{ BlockchainMessage }{valMsg})
				if !queued {
					fmt.Println("queue is full!!!")
				}
			}
		}

		for account, acceptVote := range types.AcceptVoteSet {
			valMsg := &bcValidatorResponseMessage{Account: account, Epoch: NON_EPOCH, AcceptVotes: acceptVote}
			fmt.Println("sending bcValidatorResponseMessage")
			fmt.Println("valMsg:", valMsg)
			queued := src.TrySend(BlockchainChannel, struct{ BlockchainMessage }{valMsg})
			if !queued {
				fmt.Println("queue is full!!!")
			}
		}

	case *bcValidatorResponseMessage:
		fmt.Println("received bcValidatorMessage!!!")
		fmt.Println("bcValidatorMessage:", msg)
		//TODO: here are bugs, may not handle here; should handle it in Consensus Reactor after catched up!!
		if (msg.Account != NON_ACCOUNT && msg.Epoch != NON_EPOCH) || (msg.Account == NON_ACCOUNT && msg.Epoch == NON_EPOCH) {
			fmt.Println("recevied wrong bcValidatorResponseMessage")
		} else if msg.Account != NON_ACCOUNT {
			types.ValChangedEpoch[msg.Epoch] = append(types.ValChangedEpoch[msg.Epoch], msg.AcceptVotes) //TODO validate???
		} else {
			types.AcceptVoteSet[msg.Account] = msg.AcceptVotes //TODO validate???
		}

	default:
		logger.Warn("Unknown message type ", reflect.TypeOf(msg))
	}
}

// Handle messages from the poolReactor telling the reactor what to do.
// NOTE: Don't sleep in the FOR_LOOP or otherwise slow it down!
// (Except for the SYNC_LOOP, which is the primary purpose and must be synchronous.)
func (bcR *BlockchainReactor) poolRoutine() {

	trySyncTicker := time.NewTicker(trySyncIntervalMS * time.Millisecond)
	statusUpdateTicker := time.NewTicker(statusUpdateIntervalSeconds * time.Second)
	switchToConsensusTicker := time.NewTicker(switchToConsensusIntervalSeconds * time.Second)

FOR_LOOP:
	for {
		select {
		case request := <-bcR.requestsCh: // chan BlockRequest
			peer := bcR.Switch.Peers().Get(request.PeerID)
			if peer == nil {
				continue FOR_LOOP // Peer has since been disconnected.
			}
			msg := &bcBlockRequestMessage{request.Height}
			//fmt.Printf("poolRoutine(), bcR.requestsCh with height & PeerID: %v, %v\n", request.Height, request.PeerID)
			queued := peer.TrySend(BlockchainChannel, struct{ BlockchainMessage }{msg})
			//fmt.Printf("poolRoutine(), queued is %v\n", queued);
			if !queued {
				// We couldn't make the request, send-queue full.
				// The pool handles timeouts, just let it go.
				continue FOR_LOOP
			}

			valMsg := &bcValidatorRequestMessage{}
			//fmt.Printf("poolRoutine(), bcR.requestsCh with height & PeerID: %v, %v\n", request.Height, request.PeerID)
			queued = peer.TrySend(BlockchainChannel, struct{ BlockchainMessage }{valMsg})
			//fmt.Printf("poolRoutine(), queued is %v\n", queued);
			if !queued {
				// We couldn't make the request, send-queue full.
				// The pool handles timeouts, just let it go.
				continue FOR_LOOP
			}

		case peerID := <-bcR.timeoutsCh: // chan string
			//fmt.Printf("poolRoutine(), bcR.timeoutsCh with PeerID: %v\n", peerID)
			// Peer timed out.
			peer := bcR.Switch.Peers().Get(peerID)
			if peer != nil {
				bcR.Switch.StopPeerForError(peer, errors.New("BlockchainReactor Timeout"))
			}
		case _ = <-statusUpdateTicker.C:
			//fmt.Printf("poolRoutine(), statusUpdateTicker.C\n")
			// ask for status updates
			go bcR.BroadcastStatusRequest()
		case _ = <-switchToConsensusTicker.C:
			height, numPending, _ := bcR.pool.GetStatus()
			outbound, inbound, _ := bcR.Switch.NumPeers()
			logger.Info("Consensus ticker", " numPending: ", numPending, " total: ", len(bcR.pool.requesters),
				"outbound", outbound, "inbound", inbound)
			//fmt.Printf("poolRoutine(), switchToConsensusTicker.C, numPending:%v, total:%v,outbound:%v, inbound:%v\n",
			//	numPending, len(bcR.pool.requesters), outbound, inbound)
			if bcR.pool.IsCaughtUp() {
				logger.Info("Time to switch to consensus reactor!", " height: ", height)
				bcR.pool.Stop()

				_, val, _ := bcR.state.GetValidators()
				fmt.Println("bcR.state.Validators:", val) //TODO
				conR := bcR.Switch.Reactor("CONSENSUS").(consensusReactor)
				conR.SwitchToConsensus(bcR.state)

				break FOR_LOOP
			}
		case _ = <-trySyncTicker.C: // chan time
			// This loop can be slow as long as it's doing syncing work.
			//fmt.Printf("poolRoutine(), trySyncTicker.C\n")
		SYNC_LOOP:
			for i := 0; i < 10; i++ {
				//fmt.Printf("poolRoutine(), trySyncTicker.C, in for with i:%d\n", i)
				// See if there are any blocks to sync.
				first, second := bcR.pool.PeekTwoBlocks()
				//logger.Info("TrySync peeked", "first", first, "second", second)
				if first == nil || second == nil {
					// We need both to sync the first block.
					//fmt.Printf("poolRoutine(), trySyncTicker.C, here break\n")
					break SYNC_LOOP
				}
				//fmt.Printf("poolRoutine(), first block is: %s\n", first.String())
				//fmt.Printf("poolRoutine(), second block is: %s\n", second.String())
				//fmt.Printf("poolRoutine(), Validators are: %v, LastValidators are: \n",
				//			bcR.state.Validators, bcR.state.LastValidators)
				firstParts := first.MakePartSet(bcR.config.GetInt("block_part_size")) // TODO: put part size in parts header?
				firstPartsHeader := firstParts.Header()
				// Finally, verify the first block using the second's commit
				// NOTE: we can probably make this more efficient, but note that calling
				// first.Hash() doesn't verify the tx contents, so MakePartSet() is
				// currently necessary.
				_, val, _ := bcR.state.GetValidators()
				err := val.VerifyCommit(
					bcR.state.ChainID, types.BlockID{first.Hash(), firstPartsHeader}, first.Height, second.LastCommit)
				if err != nil {
					//fmt.Printf("poolRoutine(), validators are: %s\n", bcR.state.Validators)
					logger.Info("error in validation", " error:", err)
					bcR.pool.RedoRequest(first.Height)
					break SYNC_LOOP
				} else {
					//fmt.Printf("poolRoutine(), validators are: %s\n", bcR.state.Validators)
					bcR.pool.PopRequest()

					bcR.store.SaveBlock(first, firstParts, second.LastCommit)

					// TODO: should we be firing events? need to fire NewBlock events manually ...
					// NOTE: we could improve performance if we
					// didn't make the app commit to disk every block
					// ... but we would need a way to get the hash without it persisting
					err := bcR.state.ApplyBlock(bcR.evsw, bcR.proxyAppConn, first, firstPartsHeader, types.MockMempool{})
					if err != nil {
						// TODO This is bad, are we zombie?
						cmn.PanicQ(cmn.Fmt("Failed to process committed block (%d:%X): %v", first.Height, first.Hash(), err))
					}
				}
				//fmt.Printf("poolRoutine(), trySyncTicker.C, add 2 blocks successfully\n")
			}
			continue FOR_LOOP
		case <-bcR.Quit:
			//fmt.Printf("poolRoutine(), bcR.Quit\n")
			break FOR_LOOP
		}
	}
}

// BroadcastStatusRequest broadcasts `BlockStore` height.
func (bcR *BlockchainReactor) BroadcastStatusRequest() error {
	bcR.Switch.Broadcast(BlockchainChannel, struct{ BlockchainMessage }{&bcStatusRequestMessage{bcR.store.Height()}})
	return nil
}

// SetEventSwitch implements events.Eventable
func (bcR *BlockchainReactor) SetEventSwitch(evsw types.EventSwitch) {
	bcR.evsw = evsw
}

//-----------------------------------------------------------------------------
// Messages

//modified by liaoyd
const (
	msgTypeBlockRequest   = byte(0x10)
	msgTypeBlockResponse  = byte(0x11)
	msgTypeStatusResponse = byte(0x20)
	msgTypeStatusRequest  = byte(0x21)
	msgTypeValidatorRequest      = byte(0x31)
	msgTypeValidatorResponse     = byte(0x32)
)

// BlockchainMessage is a generic message for this reactor.
type BlockchainMessage interface{}

var _ = wire.RegisterInterface(
	struct{ BlockchainMessage }{},
	wire.ConcreteType{&bcBlockRequestMessage{}, msgTypeBlockRequest},
	wire.ConcreteType{&bcBlockResponseMessage{}, msgTypeBlockResponse},
	wire.ConcreteType{&bcStatusResponseMessage{}, msgTypeStatusResponse},
	wire.ConcreteType{&bcStatusRequestMessage{}, msgTypeStatusRequest},
	wire.ConcreteType{&bcValidatorRequestMessage{}, msgTypeValidatorRequest},
	wire.ConcreteType{&bcValidatorResponseMessage{}, msgTypeValidatorResponse},
)

// DecodeMessage decodes BlockchainMessage.
// TODO: ensure that bz is completely read.
func DecodeMessage(bz []byte) (msgType byte, msg BlockchainMessage, err error) {
	msgType = bz[0]
	n := int(0)
	r := bytes.NewReader(bz)
	msg = wire.ReadBinary(struct{ BlockchainMessage }{}, r, maxBlockchainResponseSize, &n, &err).(struct{ BlockchainMessage }).BlockchainMessage
	if err != nil && n != len(bz) {
		err = errors.New("DecodeMessage() had bytes left over")
	}
	return
}

//-------------------------------------
type bcBlockRequestMessage struct {
	Height int
}

func (m *bcBlockRequestMessage) String() string {
	return cmn.Fmt("[bcBlockRequestMessage %v]", m.Height)
}

//-------------------------------------

// NOTE: keep up-to-date with maxBlockchainResponseSize
type bcBlockResponseMessage struct {
	Block *types.Block
}

func (m *bcBlockResponseMessage) String() string {
	return cmn.Fmt("[bcBlockResponseMessage %v]", m.Block.Height)
}

//-------------------------------------

type bcStatusRequestMessage struct {
	Height int
}

func (m *bcStatusRequestMessage) String() string {
	return cmn.Fmt("[bcStatusRequestMessage %v]", m.Height)
}

//-------------------------------------

type bcStatusResponseMessage struct {
	Height int
}

func (m *bcStatusResponseMessage) String() string {
	return cmn.Fmt("[bcStatusResponseMessage %v]", m.Height)
}

//----------------------------

//-------------------------------------

type bcValidatorRequestMessage struct {
}

func (m *bcValidatorRequestMessage) String() string {
	return cmn.Fmt("[bcValidatorRequestMessage")
}

//liaoyd
//send validators during fast sync from types.ValChangedHeight
type bcValidatorResponseMessage struct {
	Account string
	Epoch   int
	AcceptVotes *types.AcceptVotes
	//the above tri-tuble to represent the following structures:
	//ValChangedEpoch map[int][]*types.AcceptVotes
	//AcceptVoteSet map[string]*types.AcceptVotes
}

func (m *bcValidatorResponseMessage) String() string {
	return cmn.Fmt("[bcValidatorResponseMessage")
}
