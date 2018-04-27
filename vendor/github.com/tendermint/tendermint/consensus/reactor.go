package consensus

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"
	"strings"

	. "github.com/tendermint/go-common"
	"github.com/tendermint/go-p2p"
	"github.com/tendermint/go-wire"
	sm "github.com/tendermint/tendermint/state"
	"github.com/tendermint/tendermint/types"
	abci "github.com/tendermint/abci/types"
	"github.com/tendermint/go-clist"
)

const (
	StateChannel       = byte(0x20)
	DataChannel        = byte(0x21)
	VoteChannel        = byte(0x22)
	VoteSetBitsChannel = byte(0x23)

	peerGossipSleepDuration     = 100 * time.Millisecond // Time to sleep if there's nothing to send.
	peerQueryMaj23SleepDuration = 2 * time.Second        // Time to sleep after each VoteSetMaj23Message sent
	maxConsensusMessageSize     = 1048576                // 1MB; NOTE: keep in sync with types.PartSet sizes.
)

//-----------------------------------------------------------------------------

type ConsensusReactor struct {
	p2p.BaseReactor // BaseService + p2p.Switch

	conS     *ConsensusState
	fastSync bool
	evsw     types.EventSwitch
}

func NewConsensusReactor(consensusState *ConsensusState, fastSync bool) *ConsensusReactor {
	conR := &ConsensusReactor{
		conS:     consensusState,
		fastSync: fastSync,
	}
	conR.BaseReactor = *p2p.NewBaseReactor(logger, "ConsensusReactor", conR)
	return conR
}

func (conR *ConsensusReactor) OnStart() error {
	logger.Info("ConsensusReactor ", " fastSync:", conR.fastSync)
	conR.BaseReactor.OnStart()

	// callbacks for broadcasting new steps and votes to peers
	// upon their respective events (ie. uses evsw)
	conR.registerEventCallbacks()

	//liaoyd
	go conR.GetDiffValidator()

	if !conR.fastSync {
		// conR.catchupValidator(val, ok)
		_, err := conR.conS.Start()
		if err != nil {
			return err
		}
	}
	return nil
}

func (conR *ConsensusReactor) OnStop() {
	conR.BaseReactor.OnStop()
	conR.conS.Stop()
}

// Switch from the fast_sync to the consensus:
// reset the state, turn off fast_sync, start the consensus-state-machine
func (conR *ConsensusReactor) SwitchToConsensus(state *sm.State) {
	logger.Info("SwitchToConsensus")
	conR.conS.reconstructLastCommit(state)
	// NOTE: The line below causes broadcastNewRoundStepRoutine() to
	// broadcast a NewRoundStepMessage.
	conR.conS.updateToStateAndEpoch(state, conR.conS.epoch)
	conR.fastSync = false
	conR.conS.Start()
}

// Implements Reactor
func (conR *ConsensusReactor) GetChannels() []*p2p.ChannelDescriptor {
	// TODO optimize
	return []*p2p.ChannelDescriptor{
		&p2p.ChannelDescriptor{
			ID:                StateChannel,
			Priority:          5,
			SendQueueCapacity: 100,
		},
		&p2p.ChannelDescriptor{
			ID:                 DataChannel, // maybe split between gossiping current block and catchup stuff
			Priority:           10,          // once we gossip the whole block there's nothing left to send until next height or round
			SendQueueCapacity:  100,
			RecvBufferCapacity: 50 * 4096,
		},
		&p2p.ChannelDescriptor{
			ID:                 VoteChannel,
			Priority:           5,
			SendQueueCapacity:  100,
			RecvBufferCapacity: 100 * 100,
		},
		&p2p.ChannelDescriptor{
			ID:                 VoteSetBitsChannel,
			Priority:           1,
			SendQueueCapacity:  2,
			RecvBufferCapacity: 1024,
		},
	}
}

// Implements Reactor
func (conR *ConsensusReactor) AddPeer(peer *p2p.Peer) {
	if !conR.IsRunning() {
		return
	}

	// Create peerState for peer
	peerState := NewPeerState(peer)
	peer.Data.Set(types.PeerStateKey, peerState)

	// Begin routines for this peer.
	go conR.gossipDataRoutine(peer, peerState)
	go conR.gossipVotesRoutine(peer, peerState)
	go conR.queryMaj23Routine(peer, peerState)

	go conR.validatorExMsgRoutine(peer, peerState)
	// go conR.GetDiffValidator()

	// Send our state to peer.
	// If we're fast_syncing, broadcast a RoundStepMessage later upon SwitchToConsensus().
	if !conR.fastSync {
		conR.sendNewRoundStepMessages(peer)
	}
}

// Implements Reactor
func (conR *ConsensusReactor) RemovePeer(peer *p2p.Peer, reason interface{}) {
	if !conR.IsRunning() {
		return
	}
	// TODO
	//peer.Data.Get(PeerStateKey).(*PeerState).Disconnect()
}

// Implements Reactor
// NOTE: We process these messages even when we're fast_syncing.
// Messages affect either a peer state or the consensus state.
// Peer state updates can happen in parallel, but processing of
// proposals, block parts, and votes are ordered by the receiveRoutine
// NOTE: blocks on consensus state for proposals, block parts, and votes
func (conR *ConsensusReactor) Receive(chID byte, src *p2p.Peer, msgBytes []byte) {
	if !conR.IsRunning() {
		logger.Debug("Receive", " src:", src, " chId:", chID, " bytes:", msgBytes)
		return
	}

	_, msg, err := DecodeMessage(msgBytes)
	if err != nil {
		logger.Warn("Error decoding message", " src:", src, " chId:", chID, " msg:", msg, " error:", err, " bytes:", msgBytes)
		// TODO punish peer?
		return
	}
	logger.Debug("Receive", " src:", src, " chId:", chID, " msg:", msg)

	// Get peer states
	ps := src.Data.Get(types.PeerStateKey).(*PeerState)

	switch chID {
	case StateChannel:
		// fmt.Println(chID, src, msg)
		switch msg := msg.(type) {
		case *NewRoundStepMessage:
			ps.ApplyNewRoundStepMessage(msg)
		case *CommitStepMessage:
			ps.ApplyCommitStepMessage(msg)
		case *HasVoteMessage:
			ps.ApplyHasVoteMessage(msg)
		case *VoteSetMaj23Message:
			cs := conR.conS
			cs.mtx.Lock()
			height, votes := cs.Height, cs.Votes
			cs.mtx.Unlock()
			if height != msg.Height {
				return
			}
			// Peer claims to have a maj23 for some BlockID at H,R,S,
			votes.SetPeerMaj23(msg.Round, msg.Type, ps.Peer.Key, msg.BlockID)
			// Respond with a VoteSetBitsMessage showing which votes we have.
			// (and consequently shows which we don't have)
			var ourVotes *BitArray
			switch msg.Type {
			case types.VoteTypePrevote:
				ourVotes = votes.Prevotes(msg.Round).BitArrayByBlockID(msg.BlockID)
			case types.VoteTypePrecommit:
				ourVotes = votes.Precommits(msg.Round).BitArrayByBlockID(msg.BlockID)
			default:
				logger.Warn("Bad VoteSetBitsMessage field Type")
				return
			}
			src.TrySend(VoteSetBitsChannel, struct{ ConsensusMessage }{&VoteSetBitsMessage{
				Height:  msg.Height,
				Round:   msg.Round,
				Type:    msg.Type,
				BlockID: msg.BlockID,
				Votes:   ourVotes,
			}})
		case *TestMessage:
			fmt.Println(chID, src, msg)
			fmt.Println("get test message!!!!!!!!!")
			// fmt.Println(src.PubKey())
			switch msg.ValidatorMsg.Action {
			case "JOIN":
				// msg.ValidatorMsg.PubKey = src.PubKey()     //get PubKey
				validatorMsg := msg.ValidatorMsg
				from := validatorMsg.From
				ValidatorMsgMap[from] = validatorMsg //store request
				if _, ok := types.AcceptVoteSet[from]; !ok {
					types.AcceptVoteSet[from] = conR.NewAcceptVotes(validatorMsg, from) //new vote list to add vote
				}
			case "ACCEPT":
				conR.tryAddAcceptVote(msg.ValidatorMsg) //TODO
			}
		default:
			logger.Warn("Unknown message type ", reflect.TypeOf(msg))
		}

	case DataChannel:
		if conR.fastSync {
			logger.Warn("Ignoring message received during fastSync", " msg:", msg)
			return
		}
		switch msg := msg.(type) {
		case *ProposalMessage:
			ps.SetHasProposal(msg.Proposal)
			conR.conS.peerMsgQueue <- msgInfo{msg, src.Key}
		case *ProposalPOLMessage:
			ps.ApplyProposalPOLMessage(msg)
		case *BlockPartMessage:
			ps.SetHasProposalBlockPart(msg.Height, msg.Round, msg.Part.Index)
			conR.conS.peerMsgQueue <- msgInfo{msg, src.Key}
		case *Maj23VotesAggrMessage:
			ps.SetHasMaj23Votes(msg.Maj23VotesAggr)
			conR.conS.peerMsgQueue <- msgInfo{msg, src.Key}
		case *VotesAggrPartMessage:
			ps.SetHasMaj23VotesPart(msg.Height, msg.Round, msg.Type, msg.Part.Index)
			conR.conS.peerMsgQueue <- msgInfo{msg, src.Key}
		case *Maj23SignAggrMessage:
			ps.SetHasMaj23SignAggr(msg.SignAggr)
			conR.conS.peerMsgQueue <- msgInfo{msg, src.Key}
		default:
			logger.Warn("Unknown message type ", reflect.TypeOf(msg))
		}

	case VoteChannel:
		if conR.fastSync {
			logger.Warn("Ignoring message received during fastSync", " msg:", msg)
			return
		}
		switch msg := msg.(type) {
		case *VoteMessage:
			cs := conR.conS
			cs.mtx.Lock()
			height, valSize, lastCommitSize := cs.Height, cs.Validators.Size(), cs.LastCommit.Size()
			cs.mtx.Unlock()
			ps.EnsureVoteBitArrays(height, valSize)
			ps.EnsureVoteBitArrays(height-1, lastCommitSize)
			ps.SetHasVote(msg.Vote)

			conR.conS.peerMsgQueue <- msgInfo{msg, src.Key}

		default:
			// don't punish (leave room for soft upgrades)
			logger.Warn("Unknown message type ", reflect.TypeOf(msg))
		}

	case VoteSetBitsChannel:
		if conR.fastSync {
			logger.Warn("Ignoring message received during fastSync", " msg:", msg)
			return
		}
		switch msg := msg.(type) {
		case *VoteSetBitsMessage:
			cs := conR.conS
			cs.mtx.Lock()
			height, votes := cs.Height, cs.Votes
			cs.mtx.Unlock()

			if height == msg.Height {
				var ourVotes *BitArray
				switch msg.Type {
				case types.VoteTypePrevote:
					ourVotes = votes.Prevotes(msg.Round).BitArrayByBlockID(msg.BlockID)
				case types.VoteTypePrecommit:
					ourVotes = votes.Precommits(msg.Round).BitArrayByBlockID(msg.BlockID)
				default:
					logger.Warn("Bad VoteSetBitsMessage field Type")
					return
				}
				ps.ApplyVoteSetBitsMessage(msg, ourVotes)
			} else {
				ps.ApplyVoteSetBitsMessage(msg, nil)
			}
		default:
			// don't punish (leave room for soft upgrades)
			logger.Warn("Unknown message type ", reflect.TypeOf(msg))
		}

	default:
		logger.Warn("Unknown chId ", chID)
	}

	if err != nil {
		logger.Warn("Error in Receive()", " error:", err)
	}
}

// implements events.Eventable
func (conR *ConsensusReactor) SetEventSwitch(evsw types.EventSwitch) {
	conR.evsw = evsw
	conR.conS.SetEventSwitch(evsw)
}

//--------------------------------------

// Listens for new steps and votes,
// broadcasting the result to peers
func (conR *ConsensusReactor) registerEventCallbacks() {

	types.AddListenerForEvent(conR.evsw, "conR", types.EventStringNewRoundStep(), func(data types.TMEventData) {
		rs := data.(types.EventDataRoundState).RoundState.(*RoundState)
		conR.broadcastNewRoundStep(rs)
	})

	types.AddListenerForEvent(conR.evsw, "conR", types.EventStringVote(), func(data types.TMEventData) {
		edv := data.(types.EventDataVote)
		conR.broadcastHasVoteMessage(edv.Vote)
	})
}

func (conR *ConsensusReactor) broadcastNewRoundStep(rs *RoundState) {

	nrsMsg, csMsg := makeRoundStepMessages(rs)
	if nrsMsg != nil {
		conR.Switch.Broadcast(StateChannel, struct{ ConsensusMessage }{nrsMsg})
	}
	if csMsg != nil {
		conR.Switch.Broadcast(StateChannel, struct{ ConsensusMessage }{csMsg})
	}
}

// Broadcasts HasVoteMessage to peers that care.
func (conR *ConsensusReactor) broadcastHasVoteMessage(vote *types.Vote) {
	msg := &HasVoteMessage{
		Height: vote.Height,
		Round:  vote.Round,
		Type:   vote.Type,
		Index:  vote.ValidatorIndex,
	}
	conR.Switch.Broadcast(StateChannel, struct{ ConsensusMessage }{msg})
	/*
		// TODO: Make this broadcast more selective.
		for _, peer := range conR.Switch.Peers().List() {
			ps := peer.Data.Get(PeerStateKey).(*PeerState)
			prs := ps.GetRoundState()
			if prs.Height == vote.Height {
				// TODO: Also filter on round?
				peer.TrySend(StateChannel, struct{ ConsensusMessage }{msg})
			} else {
				// Height doesn't match
				// TODO: check a field, maybe CatchupCommitRound?
				// TODO: But that requires changing the struct field comment.
			}
		}
	*/
}

// Send HasVoteMessage to the proposer
/*
func (conR *ConsensusReactor) sendHasVoteMessage(vote *types.Vote) {
	msg := &HasVoteMessage{
		Height: vote.Height,
		Round:  vote.Round,
		Type:   vote.Type,
		Index:  vote.ValidatorIndex,
	}

	// Get proposer's peer info from the peer set
	peer := conR.Switch.Peers().Get(conR.conS.ProposerPeerKey)

	if peer == nil {
		fmt.Printf("Send HasVoteMsg: validator (peer %s vote height %d) not found proposer (peer %s).", conR.conS.ValPeerKey(), vote.Height, conR.conS.ProposerPeerKey)

		return
	} else {
		ps := peer.Data.Get(PeerStateKey).(*PeerState)
		prs := ps.GetRoundState()

		fmt.Printf("Send HasVoteMsg: validator (peer %s vote height %d), proposer (peer %s vote_height %d).", conR.conS.ValPeerKey(), vote.Height, conR.conS.ProposerPeerKey, prs.Height)

		if prs.Height == vote.Height {
			peer.TrySend(StateChannel, struct{ ConsensusMessage }{msg})
		}
	}
}
*/

func makeRoundStepMessages(rs *RoundState) (nrsMsg *NewRoundStepMessage, csMsg *CommitStepMessage) {
	nrsMsg = &NewRoundStepMessage{
		Height: rs.Height,
		Round:  rs.Round,
		Step:   rs.Step,
		SecondsSinceStartTime: int(time.Now().Sub(rs.StartTime).Seconds()),
		LastCommitRound:       rs.LastCommit.Round(),
	}
	if rs.Step == RoundStepCommit {
		csMsg = &CommitStepMessage{
			Height:           rs.Height,
			BlockPartsHeader: rs.ProposalBlockParts.Header(),
			BlockParts:       rs.ProposalBlockParts.BitArray(),
		}
	}
	return
}

func (conR *ConsensusReactor) sendNewRoundStepMessages(peer *p2p.Peer) {
	rs := conR.conS.GetRoundState()
	nrsMsg, csMsg := makeRoundStepMessages(rs)
	if nrsMsg != nil {
		peer.Send(StateChannel, struct{ ConsensusMessage }{nrsMsg})
	}
	if csMsg != nil {
		peer.Send(StateChannel, struct{ ConsensusMessage }{csMsg})
	}
}

func (conR *ConsensusReactor) gossipDataRoutine(peer *p2p.Peer, ps *PeerState) {
	//log := logger.New(" peer:", peer)

OUTER_LOOP:
	for {
		// Manage disconnects from self or peer.
		if !peer.IsRunning() || !conR.IsRunning() {
			logger.Info("Stopping gossipDataRoutine for ", peer)
			return
		}
		rs := conR.conS.GetRoundState()
		prs := ps.GetRoundState()

//		logger.Debug(Fmt("gossipDataRoutine: peer check block parts"))

		//logger.Info("gossipDataRoutine: rs.height ", rs.Height, " prs.height ", prs.Height)
		logger.Info(Fmt("gossipDataRoutine: rs.height %v prs.Height %v", rs.Height, prs.Height))

		// Send proposal Block parts?
		if rs.ProposalBlockParts.HasHeader(prs.ProposalBlockPartsHeader) {
			//logger.Info("ProposalBlockParts matched", "blockParts", prs.ProposalBlockParts)
			if index, ok := rs.ProposalBlockParts.BitArray().Sub(prs.ProposalBlockParts.Copy()).PickRandom(); ok {
				part := rs.ProposalBlockParts.GetPart(index)
				msg := &BlockPartMessage{
					Height: rs.Height, // This tells peer that this part applies to us.
					Round:  rs.Round,  // This tells peer that this part applies to us.
					Part:   part,
				}
				if peer.Send(DataChannel, struct{ ConsensusMessage }{msg}) {
					ps.SetHasProposalBlockPart(prs.Height, prs.Round, index)
				}

				logger.Debug(Fmt("gossipDataRoutine: Validator (pkey %v proposer %v) send block part (height %d round %d index %d) to peer (%s)\n", conR.conS.privValidator.GetPubKey(), conR.conS.IsProposer(), rs.Height, rs.Round, index, peer.PeerKey()))

				continue OUTER_LOOP
			}
		}

//		logger.Debug(Fmt("gossipDataRoutine: peer check prevotes parts"))

		// Send prevotes parts?
		if rs.PrevoteMaj23Parts.HasHeader(prs.PrevoteMaj23PartsHeader) {
			logger.Info("PrevoteMaj23Parts matched", "Maj23 part", prs.PrevoteMaj23Parts)
			if index, ok := rs.PrevoteMaj23Parts.BitArray().Sub(prs.PrevoteMaj23Parts.Copy()).PickRandom(); ok {
				part := rs.PrevoteMaj23Parts.GetPart(index)
				msg := &VotesAggrPartMessage{
					Height: rs.Height,
					Round:  rs.Round,
					Type:   types.VoteTypePrevote,
					Part:   part,
				}
				if peer.Send(DataChannel, struct{ ConsensusMessage }{msg}) {
					ps.SetHasMaj23VotesPart(prs.Height, prs.Round, types.VoteTypePrevote, index)
				}

				logger.Debug(Fmt("gossipDataRoutine: Validator (pkey %v proposer %v) send prevote aggr part (height %d round %d index %d) to peer %s\n", conR.conS.privValidator.GetPubKey(), conR.conS.IsProposer(), rs.Height, rs.Round, index, peer.PeerKey()))

				continue OUTER_LOOP
			}
		}

//		logger.Debug("gossipDataRoutine1", "rsHeight", rs.Height, "rsRound", rs.Round, "prsHeight", prs.Height, "prsRound", prs.Round, "prsStep", prs.Step)

//		logger.Debug("gossipDataRoutine1", "rs.PrecommitMaj23Parts header", rs.PrecommitMaj23Parts.Header(), "prs.PrecommitMaj23PartsHeader", prs.PrecommitMaj23PartsHeader)

//		logger.Debug(Fmt("gossipDataRoutine: peer check precommits parts"))

		// Send precommits parts?
		if rs.PrecommitMaj23Parts.HasHeader(prs.PrecommitMaj23PartsHeader) {
			logger.Info("PrecommiMaj23Parts matched", "Maj23 part", prs.PrecommitMaj23Parts)
			if index, ok := rs.PrecommitMaj23Parts.BitArray().Sub(prs.PrecommitMaj23Parts.Copy()).PickRandom(); ok {
				part := rs.PrecommitMaj23Parts.GetPart(index)
				msg := &VotesAggrPartMessage{
					Height: rs.Height,
					Round:  rs.Round,
					Type:   types.VoteTypePrecommit,
					Part:   part,
				}
				if peer.Send(DataChannel, struct{ ConsensusMessage }{msg}) {
					ps.SetHasMaj23VotesPart(prs.Height, prs.Round, types.VoteTypePrecommit, index)
				}

				logger.Debug(Fmt("gossipDataRoutine: Validator (pkey %v proposer %v) send precommit aggr part (height %d round %d index %d) to peer %s\n", conR.conS.privValidator.GetPubKey(), conR.conS.IsProposer(), rs.Height, rs.Round, index, peer.PeerKey()))

				continue OUTER_LOOP
			}
		}

//		logger.Debug("gossipDataRoutine2", "rsHeight", rs.Height, "rsRound", rs.Round, "prsHeight", prs.Height, "prsRound", prs.Round, "prsStep", prs.Step)
//fmt.Printf("gossipDataRoutine: peer (key %s) prs.Height %d rs.Height %d prs %#v\n", peer.PeerKey(), prs.Height, rs.Height, prs)

//		logger.Debug(Fmt("gossipDataRoutine: peer (%s) check catch up"), peer.PeerKey())

		// If the peer is on a previous height, help catch up.
		if (0 < prs.Height) && (prs.Height < rs.Height) {
			logger.Debug(Fmt("gossipDataRoutine: Validator (pkey %v proposer %v) help peer (%s) catchup (height %d round %d)\n", conR.conS.privValidator.GetPubKey(), conR.conS.IsProposer(), peer.PeerKey(), prs.Height, prs.Round))

			//logger.Info("Data catchup", " height:", rs.Height, " peerHeight:", prs.Height, "peerProposalBlockParts", prs.ProposalBlockParts)
			if index, ok := prs.ProposalBlockParts.Not().PickRandom(); ok {
				fmt.Printf("gossipDataRoutine: help peer (%s) catchup block %d\n", peer.PeerKey(), index)

				// Ensure that the peer's PartSetHeader is correct
				blockMeta := conR.conS.blockStore.LoadBlockMeta(prs.Height)
				if blockMeta == nil {
					logger.Warn("Failed to load block meta", " peer height:", prs.Height, " our height:", rs.Height, " blockstore height:", conR.conS.blockStore.Height(), " pv:", conR.conS.privValidator)
					time.Sleep(peerGossipSleepDuration)
					continue OUTER_LOOP
				} else if !blockMeta.BlockID.PartsHeader.Equals(prs.ProposalBlockPartsHeader) {
					logger.Info("Peer ProposalBlockPartsHeader mismatch, sleeping",
						" peerHeight:", prs.Height, " blockPartsHeader:", blockMeta.BlockID.PartsHeader, " peerBlockPartsHeader:", prs.ProposalBlockPartsHeader)
					time.Sleep(peerGossipSleepDuration)
					continue OUTER_LOOP
				}
				// Load the part
				part := conR.conS.blockStore.LoadBlockPart(prs.Height, index)
				if part == nil {
					logger.Warn("Could not load part", " index:", index,
						" peerHeight:", prs.Height, " blockPartsHeader:", blockMeta.BlockID.PartsHeader, " peerBlockPartsHeader:", prs.ProposalBlockPartsHeader)
					time.Sleep(peerGossipSleepDuration)
					continue OUTER_LOOP
				}
				// Send the part
				msg := &BlockPartMessage{
					Height: prs.Height, // Not our height, so it doesn't matter.
					Round:  prs.Round,  // Not our height, so it doesn't matter.
					Part:   part,
				}

				logger.Debug(Fmt("gossipDataRoutine: Validator (pkey %v proposer %v) send block part (height %d round %d index %d) to peer %v\n", conR.conS.privValidator.GetPubKey(), conR.conS.IsProposer(), prs.Height, prs.Round, index, peer.PeerKey()))

				if peer.Send(DataChannel, struct{ ConsensusMessage }{msg}) {
					ps.SetHasProposalBlockPart(prs.Height, prs.Round, index)
				}
				continue OUTER_LOOP
			} else {
				//logger.Info("No parts to send in catch-up, sleeping")
				time.Sleep(peerGossipSleepDuration)
				continue OUTER_LOOP
			}
		}

		// If height and round don't match, sleep.
		if (rs.Height != prs.Height) || (rs.Round != prs.Round) {
			//logger.Info("Peer Height|Round mismatch, sleeping", " peerHeight:", prs.Height, "peerRound", prs.Round, " peer:", peer)
			time.Sleep(peerGossipSleepDuration)
			continue OUTER_LOOP
		}

		// By here, height and round match.
		// Proposal block parts were already matched and sent if any were wanted.
		// (These can match on hash so the round doesn't matter)
		// Now consider sending other things, like the Proposal itself.

//		logger.Debug(Fmt("gossipDataRoutine: rs.Proposal %+v, prs.Proposal %+v for peer %s\n", rs.Proposal, prs.Proposal, peer.PeerKey()))

//		logger.Debug(Fmt("gossipDataRoutine: peer (%s) check send proposal"), peer.PeerKey())

		// Send Proposal && ProposalPOL BitArray?
		if rs.Proposal != nil && !prs.Proposal {
			fmt.Printf("gossipDataRoutine: Validator (pkey %v proposr %v) send proposal (height %d round %d index %d) to peer %v\n", conR.conS.privValidator.GetPubKey(), conR.conS.IsProposer(), rs.Proposal.Height, rs.Proposal.Round, peer.PeerKey())

			// Proposal: share the proposal metadata with peer.
			{
				msg := &ProposalMessage{Proposal: rs.Proposal}
				if peer.Send(DataChannel, struct{ ConsensusMessage }{msg}) {
					ps.SetHasProposal(rs.Proposal)
				}
			}
			// ProposalPOL: lets peer know which POL votes we have so far.
			// Peer must receive ProposalMessage first.
			// rs.Proposal was validated, so rs.Proposal.POLRound <= rs.Round,
			// so we definitely have rs.Votes.Prevotes(rs.Proposal.POLRound).
			if 0 <= rs.Proposal.POLRound {
				msg := &ProposalPOLMessage{
					Height:           rs.Height,
					ProposalPOLRound: rs.Proposal.POLRound,
					ProposalPOL:      rs.Votes.Prevotes(rs.Proposal.POLRound).BitArray(),
				}
				peer.Send(DataChannel, struct{ ConsensusMessage }{msg})

				logger.Debug(Fmt("gossipDataRoutine: Validator (pkey %v proposer %v) send POL (height %d round %d index %d) to peer %v\n", conR.conS.privValidator.GetPubKey(), rs.Height, rs.Proposal.POLRound, peer.PeerKey()))
			}
			continue OUTER_LOOP
		}

//		logger.Debug(Fmt("gossipDataRoutine: peer check send PrevoteAggr"))

		// Send prevote 2/3+ vote set metadata?
		if rs.PrevoteAggr != nil && !prs.PrevoteAggr {
			logger.Debug(Fmt("gossipDataRoutine: Validator (addr %v proposr %v) send prevoteAggr (height %d round %d index %d) to peer %v\n", conR.conS.privValidator.GetAddress(), conR.conS.IsProposer(), rs.Proposal.Height, rs.Proposal.Round))

			// share the Prevote VotesAggr metadata with peer.
			{
				msg := &Maj23VotesAggrMessage{Maj23VotesAggr: rs.PrevoteAggr}
				if peer.Send(DataChannel, struct{ ConsensusMessage }{msg}) {
					ps.SetHasMaj23Votes(rs.PrevoteAggr)
				}
			}
			continue OUTER_LOOP
		}

//		logger.Debug(Fmt("gossipDataRoutine: rs.PrecommitAggr %+v, prs.PrecommitAggr %+v for peer %s\n", rs.PrecommitAggr, prs.PrecommitAggr, peer.PeerKey()))

//		logger.Debug(Fmt("gossipDataRoutine: peer (%s) check send PrecommitAggr"), peer.PeerKey())

		// Send precommit 2/3+ vote set metadata?
		if rs.PrecommitAggr != nil && !prs.PrecommitAggr {
			logger.Debug(Fmt("gossipDataRoutine: Validator (pkey %v proposr %v) send precommitAggr (height %d round %d index %d) to peer %v\n", conR.conS.privValidator.GetPubKey(), conR.conS.IsProposer(), rs.Proposal.Height, rs.Proposal.Round, peer.PeerKey()))

			// share the Precommit VotesAggr metadata with peer.
			{
				msg := &Maj23VotesAggrMessage{Maj23VotesAggr: rs.PrecommitAggr}
				if peer.Send(DataChannel, struct{ ConsensusMessage }{msg}) {
					ps.SetHasMaj23Votes(rs.PrecommitAggr)
				}
			}
			continue OUTER_LOOP
		}

		// Send prevote 2/3+ BLS signature aggregation
		if rs.PrevoteMaj23SignAggr != nil && !prs.PrevoteMaj23SignAggr {
			logger.Debug(Fmt("gossipDataRoutine: Validator (addr %v proposr %v) send prevoteAggr (height %d round %d index %d) to peer %v\n", conR.conS.privValidator.GetAddress(), conR.conS.IsProposer(), rs.Proposal.Height, rs.Proposal.Round))

			// share the Prevote signature aggregation with peer.
			{
				msg := &Maj23SignAggrMessage{Maj23SignAggr: rs.PrevoteMaj23SignAggr}
				if peer.Send(DataChannel, struct{ ConsensusMessage }{msg}) {
					ps.SetHasMaj23SignAggr(rs.PrevoteMaj23SignAggr)
				}
			}
			continue OUTER_LOOP
		}

		// Send precommit 2/3+ BLS signature aggregation
		if rs.PrecommitMaj23SignAggr != nil && !prs.PrecommitMaj23SignAggr {
			logger.Debug(Fmt("gossipDataRoutine: Validator (addr %v proposr %v) send prevoteAggr (height %d round %d index %d) to peer %v\n", conR.conS.privValidator.GetAddress(), conR.conS.IsProposer(), rs.Proposal.Height, rs.Proposal.Round))

			// share the Prevote signature aggregation with peer.
			{
				msg := &Maj23SignAggrMessage{Maj23SignAggr: rs.PrecommitMaj23SignAggr}
				if peer.Send(DataChannel, struct{ ConsensusMessage }{msg}) {
					ps.SetHasMaj23SignAggr(rs.PrecommitMaj23SignAggr)
				}
			}
			continue OUTER_LOOP
		}

		//logger.Debug(Fmt("gossipDataRoutine: peer has nothing to do"))

		// Nothing to do. Sleep.
		time.Sleep(peerGossipSleepDuration)
		continue OUTER_LOOP
	}
}

func (conR *ConsensusReactor) gossipVotesRoutine(peer *p2p.Peer, ps *PeerState) {
	//log := logger.New(" peer:", peer)

	// Simple hack to throttle logs upon sleep.
	var sleeping = 0

OUTER_LOOP:
	for {
		// Manage disconnects from self or peer.
		if !peer.IsRunning() || !conR.IsRunning() {
			logger.Info("Stopping gossipVotesRoutine for ", peer)
			return
		}
		rs := conR.conS.GetRoundState()
		prs := ps.GetRoundState()

		switch sleeping {
		case 1: // First sleep
			sleeping = 2
		case 2: // No more sleep
			sleeping = 0
		}

		// Handle different cases
		// 1. Current validator is proposer, don't send vote (only send after 2/3+ votes received)
		// 2. Current validator is not validator, it has several peers, only send vote to proposer
/*
		if conR.conS.IsProposer() {
			if conR.conS.privValidator == nil {
				panic("conR.conS.privValidator is nil")
			}

			logger.Debug(Fmt("gossipVoteRoutine: Validator (pubkey %v) is proposer, not send vote (height %d round %d)\n", conR.conS.privValidator.GetPubKey(), rs.Height, rs.Round))

			time.Sleep(peerGossipSleepDuration)
			continue OUTER_LOOP
		} else
*/
		 {
			if conR.conS.privValidator == nil {
				panic("conR.conS.privValidator is nil")
			}

			logger.Debug(Fmt("gossipVoteRoutine: Validator (pubkey %v) proposer (%v) for (height %d round %d)\n", conR.conS.privValidator.GetPubKey(), conR.conS.IsProposer(), rs.Height, rs.Round))

			if strings.Compare(peer.PeerKey(), conR.conS.ProposerPeerKey) != 0 {
				logger.Debug(Fmt("gossipVoteRoutine: Peer (%s) is not proposer (key %s) on validator (pubkey %v) not send vote (height %d round %d)\n", peer.PeerKey(), conR.conS.ProposerPeerKey, conR.conS.privValidator.GetPubKey(), rs.Height, rs.Round))

				time.Sleep(peerGossipSleepDuration)
				continue OUTER_LOOP
			} else {
				logger.Debug(Fmt("gossipVoteRoutine: I'am proposer (this peer key %s, propoer peer key %s) of current validator (pubkey %v) send vote (height %d round %d)\n", peer.PeerKey(), conR.conS.ProposerPeerKey, conR.conS.privValidator.GetPubKey(), rs.Height, rs.Round))
			}
		}

		//logger.Debug("gossipVotesRoutine", "rsHeight", rs.Height, "rsRound", rs.Round,
		//	"prsHeight", prs.Height, "prsRound", prs.Round, "prsStep", prs.Step)

		// If height matches, then send LastCommit, Prevotes, Precommits.
		if rs.Height == prs.Height {
			// If there are lastCommits to send...
			if prs.Step == RoundStepNewHeight {
				if ps.PickSendVote(rs.LastCommit) {
					logger.Debug("Picked rs.LastCommit to send")
					continue OUTER_LOOP
				}
			}
			// If there are prevotes to send...
			if prs.Step <= RoundStepPrevote && prs.Round != -1 && prs.Round <= rs.Round {
				logger.Debug("Try to pick rs.Prevotes(prs.Round) to send")

				if ps.PickSendVote(rs.Votes.Prevotes(prs.Round)) {
					logger.Debug("Picked rs.Prevotes(prs.Round) to send")
					continue OUTER_LOOP
				}
			}
			// If there are precommits to send...
			if prs.Step <= RoundStepPrecommit && prs.Round != -1 && prs.Round <= rs.Round {
				logger.Debug("Try to pick rs.Precommits(prs.Round) to send")

				if ps.PickSendVote(rs.Votes.Precommits(prs.Round)) {
					logger.Debug("Picked rs.Precommits(prs.Round) to send")
					continue OUTER_LOOP
				}
			}
			// If there are POLPrevotes to send...
			if prs.ProposalPOLRound != -1 {
				if polPrevotes := rs.Votes.Prevotes(prs.ProposalPOLRound); polPrevotes != nil {
					if ps.PickSendVote(polPrevotes) {
						logger.Debug("Picked rs.Prevotes(prs.ProposalPOLRound) to send")
						continue OUTER_LOOP
					}
				}
			}
			logger.Debug("gossipVotesRoutine: no vote to send")
		}

		// Special catchup logic.
		// If peer is lagging by height 1, send LastCommit.
		if prs.Height != 0 && rs.Height == prs.Height+1 {
			if ps.PickSendVote(rs.LastCommit) {
				logger.Debug("Picked rs.LastCommit to send")
				continue OUTER_LOOP
			}
		}

		// Catchup logic
		// If peer is lagging by more than 1, send Commit.
		if prs.Height != 0 && rs.Height >= prs.Height+2 {
			// Load the block commit for prs.Height,
			// which contains precommit signatures for prs.Height.
			commit := conR.conS.blockStore.LoadBlockCommit(prs.Height)
			logger.Info("Loaded BlockCommit for catch-up", " height:", prs.Height, " commit:", commit)
			if ps.PickSendVote(commit) {
				logger.Debug("Picked Catchup commit to send")
				continue OUTER_LOOP
			}
		}

		if sleeping == 0 {
			// We sent nothing. Sleep...
			sleeping = 1
			logger.Debug("No votes to send, sleeping", " peer:", peer,
				"localPV", rs.Votes.Prevotes(rs.Round).BitArray(), "peerPV", prs.Prevotes,
				"localPC", rs.Votes.Precommits(rs.Round).BitArray(), "peerPC", prs.Precommits)
		} else if sleeping == 2 {
			// Continued sleep...
			logger.Debug("gossipVoteRoutine:: No votes to send, continue sleeping")
			sleeping = 1
		}

		time.Sleep(peerGossipSleepDuration)
		continue OUTER_LOOP
	}
}

// NOTE: `queryMaj23Routine` has a simple crude design since it only comes
// into play for liveness when there's a signature DDoS attack happening.
func (conR *ConsensusReactor) queryMaj23Routine(peer *p2p.Peer, ps *PeerState) {
	//log := logger.New(" peer:", peer)

OUTER_LOOP:
	for {
		// Manage disconnects from self or peer.
		if !peer.IsRunning() || !conR.IsRunning() {
			logger.Info("Stopping queryMaj23Routine for ", peer)
			return
		}

		// Maybe send Height/Round/Prevotes
		{
			rs := conR.conS.GetRoundState()
			prs := ps.GetRoundState()
			if rs.Height == prs.Height {
				if maj23, ok := rs.Votes.Prevotes(prs.Round).TwoThirdsMajority(); ok {
					peer.TrySend(StateChannel, struct{ ConsensusMessage }{&VoteSetMaj23Message{
						Height:  prs.Height,
						Round:   prs.Round,
						Type:    types.VoteTypePrevote,
						BlockID: maj23,
					}})
					time.Sleep(peerQueryMaj23SleepDuration)
				}
			}
		}

		// Maybe send Height/Round/Precommits
		{
			rs := conR.conS.GetRoundState()
			prs := ps.GetRoundState()
			if rs.Height == prs.Height {
				if maj23, ok := rs.Votes.Precommits(prs.Round).TwoThirdsMajority(); ok {
					peer.TrySend(StateChannel, struct{ ConsensusMessage }{&VoteSetMaj23Message{
						Height:  prs.Height,
						Round:   prs.Round,
						Type:    types.VoteTypePrecommit,
						BlockID: maj23,
					}})
					time.Sleep(peerQueryMaj23SleepDuration)
				}
			}
		}

		// Maybe send Height/Round/ProposalPOL
		{
			rs := conR.conS.GetRoundState()
			prs := ps.GetRoundState()
			if rs.Height == prs.Height && prs.ProposalPOLRound >= 0 {
				if maj23, ok := rs.Votes.Prevotes(prs.ProposalPOLRound).TwoThirdsMajority(); ok {
					peer.TrySend(StateChannel, struct{ ConsensusMessage }{&VoteSetMaj23Message{
						Height:  prs.Height,
						Round:   prs.ProposalPOLRound,
						Type:    types.VoteTypePrevote,
						BlockID: maj23,
					}})
					time.Sleep(peerQueryMaj23SleepDuration)
				}
			}
		}

		// Little point sending LastCommitRound/LastCommit,
		// These are fleeting and non-blocking.

		// Maybe send Height/CatchupCommitRound/CatchupCommit.
		{
			prs := ps.GetRoundState()
			if prs.CatchupCommitRound != -1 && 0 < prs.Height && prs.Height <= conR.conS.blockStore.Height() {
				commit := conR.conS.LoadCommit(prs.Height)
				peer.TrySend(StateChannel, struct{ ConsensusMessage }{&VoteSetMaj23Message{
					Height:  prs.Height,
					Round:   commit.Round(),
					Type:    types.VoteTypePrecommit,
					BlockID: commit.BlockID,
				}})
				time.Sleep(peerQueryMaj23SleepDuration)
			}
		}

		time.Sleep(peerQueryMaj23SleepDuration)

		continue OUTER_LOOP
	}
}

func (conR *ConsensusReactor) String() string {
	// better not to access shared variables
	return "ConsensusReactor" // conR.StringIndented("")
}

func (conR *ConsensusReactor) StringIndented(indent string) string {
	s := "ConsensusReactor{\n"
	s += indent + "  " + conR.conS.StringIndented(indent+"  ") + "\n"
	for _, peer := range conR.Switch.Peers().List() {
		ps := peer.Data.Get(types.PeerStateKey).(*PeerState)
		s += indent + "  " + ps.StringIndented(indent+"  ") + "\n"
	}
	s += indent + "}"
	return s
}

//-----------------------------------------------------------------------------

// Read only when returned by PeerState.GetRoundState().
type PeerRoundState struct {
	Height                   int                 // Height peer is at
	Round                    int                 // Round peer is at, -1 if unknown.
	Step                     RoundStepType       // Step peer is at
	StartTime                time.Time           // Estimated start of round 0 at this height
	Proposal                 bool                // True if peer has proposal for this round
	ProposalBlockPartsHeader types.PartSetHeader //
	ProposalBlockParts       *BitArray           //
	ProposalPOLRound         int                 // Proposal's POL round. -1 if none.
	ProposalPOL              *BitArray           // nil until ProposalPOLMessage received.
	Prevotes                 *BitArray           // All votes peer has for this round
	Precommits               *BitArray           // All precommits peer has for this round
	LastCommitRound          int                 // Round of commit for last height. -1 if none.
	LastCommit               *BitArray           // All commit precommits of commit for last height.
	CatchupCommitRound       int                 // Round that we have commit for. Not necessarily unique. -1 if none.
	CatchupCommit            *BitArray           // All commit precommits peer has for this height & CatchupCommitRound
	PrevoteAggr		 bool
	PrevoteMaj23PartsHeader  types.PartSetHeader //
	PrevoteMaj23Parts        *BitArray           //
	PrecommitAggr		 bool
	PrecommitMaj23PartsHeader  types.PartSetHeader //
	PrecommitMaj23Parts        *BitArray           //

	// Fields used for BLS signature aggregation
	PrevoteMaj23SignAggr	bool
	PrecommitMaj23SignAggr	bool
}

func (prs PeerRoundState) String() string {
	return prs.StringIndented("")
}

func (prs PeerRoundState) StringIndented(indent string) string {
	return fmt.Sprintf(`PeerRoundState{
%s  %v/%v/%v @%v
%s  Proposal %v -> %v
%s  POL      %v (round %v)
%s  Prevotes   %v
%s  Precommits %v
%s  LastCommit %v (round %v)
%s  Catchup    %v (round %v)
%s}`,
		indent, prs.Height, prs.Round, prs.Step, prs.StartTime,
		indent, prs.ProposalBlockPartsHeader, prs.ProposalBlockParts,
		indent, prs.ProposalPOL, prs.ProposalPOLRound,
		indent, prs.Prevotes,
		indent, prs.Precommits,
		indent, prs.LastCommit, prs.LastCommitRound,
		indent, prs.CatchupCommit, prs.CatchupCommitRound,
		indent)
}

//-----------------------------------------------------------------------------

var (
	ErrPeerStateHeightRegression = errors.New("Error peer state height regression")
	ErrPeerStateInvalidStartTime = errors.New("Error peer state invalid startTime")
)

type PeerState struct {
	Peer *p2p.Peer

	mtx sync.Mutex
	PeerRoundState
}

func NewPeerState(peer *p2p.Peer) *PeerState {
	return &PeerState{
		Peer: peer,
		PeerRoundState: PeerRoundState{
			Round:              -1,
			ProposalPOLRound:   -1,
			LastCommitRound:    -1,
			CatchupCommitRound: -1,
		},
	}
}

// Returns an atomic snapshot of the PeerRoundState.
// There's no point in mutating it since it won't change PeerState.
func (ps *PeerState) GetRoundState() *PeerRoundState {
	ps.mtx.Lock()
	defer ps.mtx.Unlock()

	prs := ps.PeerRoundState // copy
	return &prs
}

// Returns an atomic snapshot of the PeerRoundState's height
// used by the mempool to ensure peers are caught up before broadcasting new txs
func (ps *PeerState) GetHeight() int {
	ps.mtx.Lock()
	defer ps.mtx.Unlock()
	return ps.PeerRoundState.Height
}

func (ps *PeerState) SetHasProposal(proposal *types.Proposal) {
	ps.mtx.Lock()
	defer ps.mtx.Unlock()

	if ps.Height != proposal.Height || ps.Round != proposal.Round {
		return
	}
	if ps.Proposal {
		return
	}

	logger.Debug(Fmt("SetHasProposal: Peer got proposal : %#v", proposal))

	ps.Proposal = true
	ps.ProposalBlockPartsHeader = proposal.BlockPartsHeader
	ps.ProposalBlockParts = NewBitArray(proposal.BlockPartsHeader.Total)
	ps.ProposalPOLRound = proposal.POLRound
	ps.ProposalPOL = nil // Nil until ProposalPOLMessage received.

	logger.Debug(Fmt("SetHasProposal: reset PrevoteAggr %v %v", ps.PrevoteAggr, ps.PrevoteMaj23PartsHeader))
	logger.Debug(Fmt("SetHasProposal: reset PrecommitAggr %v %v", ps.PrecommitAggr, ps.PrecommitMaj23PartsHeader))

	ps.PrevoteAggr = false
	ps.PrevoteMaj23PartsHeader = types.PartSetHeader{}
	ps.PrevoteMaj23Parts = nil
	ps.PrecommitAggr = false
	ps.PrecommitMaj23PartsHeader = types.PartSetHeader{}
	ps.PrecommitMaj23Parts = nil
}

func (ps *PeerState) SetHasProposalBlockPart(height int, round int, index int) {
	ps.mtx.Lock()
	defer ps.mtx.Unlock()

	if ps.Height != height || ps.Round != round {
		return
	}

	ps.ProposalBlockParts.SetIndex(index, true)
}

// Received msg saying proposer has 2/3+ votes, votes should come later
func (ps *PeerState) SetHasMaj23Votes(votesAggr *types.VotesAggr) {
	ps.mtx.Lock()
	defer ps.mtx.Unlock()

	if ps.Height != votesAggr.Height || ps.Round != votesAggr.Round {
		return
	}

	if votesAggr.Type == types.VoteTypePrevote {
		if ps.PrevoteAggr {
			return
		}

		ps.PrevoteAggr = true
		ps.PrevoteMaj23PartsHeader = votesAggr.VotePartsHeader
		ps.PrevoteMaj23Parts = NewBitArray(votesAggr.VotePartsHeader.Total)
	} else if votesAggr.Type == types.VoteTypePrecommit {
		if ps.PrecommitAggr {
			return
		}

		ps.PrecommitAggr = true
		ps.PrecommitMaj23PartsHeader = votesAggr.VotePartsHeader
		ps.PrecommitMaj23Parts = NewBitArray(votesAggr.VotePartsHeader.Total)

fmt.Printf("SetHasMaj23Votes: peer (%s) set up ps.PrecommitMaj23Parts, header %#v\n", ps.Peer.PeerKey(), ps.PrecommitMaj23PartsHeader)
	} else {
		panic(Fmt("Invalid VotesAggr type %d", votesAggr.Type))
	}
}

// Received VotesAggr part, save it based on the type
func (ps *PeerState) SetHasMaj23VotesPart(height int, round int, votetype byte, index int) {
	if votetype == types.VoteTypePrevote {
		ps.SetHasMaj23PrevotePart(height, round, index)
	} else if votetype == types.VoteTypePrecommit {
		ps.SetHasMaj23PrecommitPart(height, round, index)
	}
}

// Received VotesAggr part for prevote, save it
func (ps *PeerState) SetHasMaj23PrevotePart(height int, round int, index int) {
	ps.mtx.Lock()
	defer ps.mtx.Unlock()

	if ps.Height != height || ps.Round != round {
		return
	}

	ps.PrevoteMaj23Parts.SetIndex(index, true)
}

// Received VotesAggr part for precommit, save it
func (ps *PeerState) SetHasMaj23PrecommitPart(height int, round int, index int) {
fmt.Printf("enter SetHasMaj23PrecommitPart: part %d\n", index)
	ps.mtx.Lock()
	defer ps.mtx.Unlock()

	if ps.Height != height || ps.Round != round {
		return
	}

	ps.PrecommitMaj23Parts.SetIndex(index, true)
}

// Received msg saying proposer has 2/3+ votes including the signature aggregation
func (ps *PeerState) SetHasMaj23SignAggr(signAggr *types.SignAggr) {
	ps.mtx.Lock()
	defer ps.mtx.Unlock()

	if ps.Height != signAggr.Height || ps.Round != signAggr.Round {
		return
	}

	if signAggr.Type == types.VoteTypePrevote {
		if ps.PrevoteMaj23SignAggr {
			return
		}

		ps.PrevoteMaj23SignAggr = true
//		ps.PrevoteMaj23PartsHeader = signAggr.VotePartsHeader
	} else if votesAggr.TypeMaj23Sign == types.VoteTypePrecommit {
		if ps.PrecommitMaj23SignAggr {
			return
		}

		ps.PrecommitAggr = true
//		ps.PrecommitMaj23PartsHeader = votesAggr.VotePartsHeader

//		fmt.Printf("SetHasMaj23SignAggr: peer (%s) set up ps.PrecommitMaj23SignAggr %#v\n", ps.Peer.PeerKey(), ps.PrecommitMaj23SignAggr)
	} else {
		panic(Fmt("Invalid signAggr type %d", signAggr.Type))
	}
}

// PickVoteToSend sends vote to peer.
// Returns true if vote was sent.
func (ps *PeerState) PickSendVote(votes types.VoteSetReader) (ok bool) {
	if vote, ok := ps.PickVoteToSend(votes); ok {
		msg := &VoteMessage{vote}
		return ps.Peer.Send(VoteChannel, struct{ ConsensusMessage }{msg})
	}
	return false
}

// votes: Must be the correct Size() for the Height().
func (ps *PeerState) PickVoteToSend(votes types.VoteSetReader) (vote *types.Vote, ok bool) {
	ps.mtx.Lock()
	defer ps.mtx.Unlock()

	if votes.Size() == 0 {
		return nil, false
	}

	height, round, type_, size := votes.Height(), votes.Round(), votes.Type(), votes.Size()

	// Lazily set data using 'votes'.
	if votes.IsCommit() {
		ps.ensureCatchupCommitRound(height, round, size)
	}
	ps.ensureVoteBitArrays(height, size)

	psVotes := ps.getVoteBitArray(height, round, type_)
	if psVotes == nil {
		return nil, false // Not something worth sending
	}
	if index, ok := votes.BitArray().Sub(psVotes).PickRandom(); ok {
		ps.setHasVote(height, round, type_, index)
		return votes.GetByIndex(index), true
	}
	return nil, false
}

func (ps *PeerState) getVoteBitArray(height, round int, type_ byte) *BitArray {
	if !types.IsVoteTypeValid(type_) {
		PanicSanity("Invalid vote type")
	}

	if ps.Height == height {
		if ps.Round == round {
			switch type_ {
			case types.VoteTypePrevote:
				return ps.Prevotes
			case types.VoteTypePrecommit:
				return ps.Precommits
			}
		}
		if ps.CatchupCommitRound == round {
			switch type_ {
			case types.VoteTypePrevote:
				return nil
			case types.VoteTypePrecommit:
				return ps.CatchupCommit
			}
		}
		if ps.ProposalPOLRound == round {
			switch type_ {
			case types.VoteTypePrevote:
				return ps.ProposalPOL
			case types.VoteTypePrecommit:
				return nil
			}
		}
		return nil
	}
	if ps.Height == height+1 {
		if ps.LastCommitRound == round {
			switch type_ {
			case types.VoteTypePrevote:
				return nil
			case types.VoteTypePrecommit:
				return ps.LastCommit
			}
		}
		return nil
	}
	return nil
}

// 'round': A round for which we have a +2/3 commit.
func (ps *PeerState) ensureCatchupCommitRound(height, round int, numValidators int) {
	if ps.Height != height {
		return
	}
	/*
		NOTE: This is wrong, 'round' could change.
		e.g. if orig round is not the same as block LastCommit round.
		if ps.CatchupCommitRound != -1 && ps.CatchupCommitRound != round {
			PanicSanity(Fmt("Conflicting CatchupCommitRound. Height: %v, Orig: %v, New: %v", height, ps.CatchupCommitRound, round))
		}
	*/
	if ps.CatchupCommitRound == round {
		return // Nothing to do!
	}
	ps.CatchupCommitRound = round
	if round == ps.Round {
		ps.CatchupCommit = ps.Precommits
	} else {
		ps.CatchupCommit = NewBitArray(numValidators)
	}
}

// NOTE: It's important to make sure that numValidators actually matches
// what the node sees as the number of validators for height.
func (ps *PeerState) EnsureVoteBitArrays(height int, numValidators int) {
	ps.mtx.Lock()
	defer ps.mtx.Unlock()
	ps.ensureVoteBitArrays(height, numValidators)
}

func (ps *PeerState) ensureVoteBitArrays(height int, numValidators int) {
	if ps.Height == height {
		if ps.Prevotes == nil {
			ps.Prevotes = NewBitArray(numValidators)
		}
		if ps.Precommits == nil {
			ps.Precommits = NewBitArray(numValidators)
		}
		if ps.CatchupCommit == nil {
			ps.CatchupCommit = NewBitArray(numValidators)
		}
		if ps.ProposalPOL == nil {
			ps.ProposalPOL = NewBitArray(numValidators)
		}
	} else if ps.Height == height+1 {
		if ps.LastCommit == nil {
			ps.LastCommit = NewBitArray(numValidators)
		}
	}
}

func (ps *PeerState) SetHasVote(vote *types.Vote) {
	ps.mtx.Lock()
	defer ps.mtx.Unlock()

	ps.setHasVote(vote.Height, vote.Round, vote.Type, vote.ValidatorIndex)
}

func (ps *PeerState) setHasVote(height int, round int, type_ byte, index int) {
	//log := logger.New(" peer:", ps.Peer, "peerRound", ps.Round, " height:", height, "round", round)
	logger.Debug("setHasVote lastCommit", ps.LastCommit, " index:", index)

	// NOTE: some may be nil BitArrays -> no side effects.
	ps.getVoteBitArray(height, round, type_).SetIndex(index, true)
}

func (ps *PeerState) ApplyNewRoundStepMessage(msg *NewRoundStepMessage) {
	ps.mtx.Lock()
	defer ps.mtx.Unlock()

	// Ignore duplicates or decreases
	if CompareHRS(msg.Height, msg.Round, msg.Step, ps.Height, ps.Round, ps.Step) <= 0 {
		return
	}

	// Just remember these values.
	psHeight := ps.Height
	psRound := ps.Round
	//psStep := ps.Step
	psCatchupCommitRound := ps.CatchupCommitRound
	psCatchupCommit := ps.CatchupCommit

	startTime := time.Now().Add(-1 * time.Duration(msg.SecondsSinceStartTime) * time.Second)
	ps.Height = msg.Height
	ps.Round = msg.Round
	ps.Step = msg.Step
	ps.StartTime = startTime
	if psHeight != msg.Height || psRound != msg.Round {
		ps.Proposal = false
		ps.ProposalBlockPartsHeader = types.PartSetHeader{}
		ps.ProposalBlockParts = nil
		ps.ProposalPOLRound = -1
		ps.ProposalPOL = nil
		// We'll update the BitArray capacity later.
		ps.Prevotes = nil
		ps.Precommits = nil

/*
		logger.Debug(Fmt("ApplyNewRoundStepMessage: psHeight %d msg.Height %d psRound %d msg.Round", psHeight, msg.Height, psRound, msg.Round))
		logger.Debug(Fmt("ApplyNewRoundStepMessage: reset PrevoteAggr %v %v", ps.PrevoteAggr, ps.PrevoteMaj23PartsHeader))
		logger.Debug(Fmt("ApplyNewRoundStepMessage: reset PrecommitAggr", ps.PrecommitAggr, ps.PrecommitMaj23PartsHeader))

		ps.PrevoteAggr = false
		ps.PrevoteMaj23PartsHeader = types.PartSetHeader{}
		ps.PrevoteMaj23Parts = nil
		ps.PrecommitAggr = false
		ps.PrecommitMaj23PartsHeader = types.PartSetHeader{}
		ps.PrecommitMaj23Parts = nil
*/
	}
	if psHeight == msg.Height && psRound != msg.Round && msg.Round == psCatchupCommitRound {
		// Peer caught up to CatchupCommitRound.
		// Preserve psCatchupCommit!
		// NOTE: We prefer to use prs.Precommits if
		// pr.Round matches pr.CatchupCommitRound.
		ps.Precommits = psCatchupCommit
	}
	if psHeight != msg.Height {
		// Shift Precommits to LastCommit.
		if psHeight+1 == msg.Height && psRound == msg.LastCommitRound {
			ps.LastCommitRound = msg.LastCommitRound
			ps.LastCommit = ps.Precommits
		} else {
			ps.LastCommitRound = msg.LastCommitRound
			ps.LastCommit = nil
		}
		// We'll update the BitArray capacity later.
		ps.CatchupCommitRound = -1
		ps.CatchupCommit = nil
	}
}

func (ps *PeerState) ApplyCommitStepMessage(msg *CommitStepMessage) {
	ps.mtx.Lock()
	defer ps.mtx.Unlock()

	if ps.Height != msg.Height {
		return
	}

	ps.ProposalBlockPartsHeader = msg.BlockPartsHeader
	ps.ProposalBlockParts = msg.BlockParts
}

func (ps *PeerState) ApplyProposalPOLMessage(msg *ProposalPOLMessage) {
	ps.mtx.Lock()
	defer ps.mtx.Unlock()

	if ps.Height != msg.Height {
		return
	}
	if ps.ProposalPOLRound != msg.ProposalPOLRound {
		return
	}

	// TODO: Merge onto existing ps.ProposalPOL?
	// We might have sent some prevotes in the meantime.
	ps.ProposalPOL = msg.ProposalPOL
}

func (ps *PeerState) ApplyHasVoteMessage(msg *HasVoteMessage) {
	ps.mtx.Lock()
	defer ps.mtx.Unlock()

	if ps.Height != msg.Height {
		return
	}

	ps.setHasVote(msg.Height, msg.Round, msg.Type, msg.Index)
}

// The peer has responded with a bitarray of votes that it has
// of the corresponding BlockID.
// ourVotes: BitArray of votes we have for msg.BlockID
// NOTE: if ourVotes is nil (e.g. msg.Height < rs.Height),
// we conservatively overwrite ps's votes w/ msg.Votes.
func (ps *PeerState) ApplyVoteSetBitsMessage(msg *VoteSetBitsMessage, ourVotes *BitArray) {
	ps.mtx.Lock()
	defer ps.mtx.Unlock()

	votes := ps.getVoteBitArray(msg.Height, msg.Round, msg.Type)
	if votes != nil {
		if ourVotes == nil {
			votes.Update(msg.Votes)
		} else {
			otherVotes := votes.Sub(ourVotes)
			hasVotes := otherVotes.Or(msg.Votes)
			votes.Update(hasVotes)
		}
	}
}

func (ps *PeerState) String() string {
	return ps.StringIndented("")
}

func (ps *PeerState) StringIndented(indent string) string {
	return fmt.Sprintf(`PeerState{
%s  Key %v
%s  PRS %v
%s}`,
		indent, ps.Peer.Key,
		indent, ps.PeerRoundState.StringIndented(indent+"  "),
		indent)
}

//-----------------------------------------------------------------------------
// Messages

const (
	msgTypeNewRoundStep = byte(0x01)
	msgTypeCommitStep   = byte(0x02)
	msgTypeProposal     = byte(0x11)
	msgTypeProposalPOL  = byte(0x12)
	msgTypeBlockPart    = byte(0x13) // both block & POL
	msgTypeVote         = byte(0x14)
	msgTypeHasVote      = byte(0x15)
	msgTypeVoteSetMaj23 = byte(0x16)
	msgTypeVoteSetBits  = byte(0x17)
	msgTypeMaj23VotesAggr= byte(0x18)
	msgTypeVotesAggrPart = byte(0x19)
	msgTypeMaj23SignAggr = byte(0x1a)
	//new liaoyd
	msgTypeTest = byte(0x03)
)

type ConsensusMessage interface{}

var _ = wire.RegisterInterface(
	struct{ ConsensusMessage }{},
	wire.ConcreteType{&NewRoundStepMessage{}, msgTypeNewRoundStep},
	wire.ConcreteType{&CommitStepMessage{}, msgTypeCommitStep},
	wire.ConcreteType{&ProposalMessage{}, msgTypeProposal},
	wire.ConcreteType{&ProposalPOLMessage{}, msgTypeProposalPOL},
	wire.ConcreteType{&BlockPartMessage{}, msgTypeBlockPart},
	wire.ConcreteType{&VoteMessage{}, msgTypeVote},
	wire.ConcreteType{&HasVoteMessage{}, msgTypeHasVote},
	wire.ConcreteType{&VoteSetMaj23Message{}, msgTypeVoteSetMaj23},
	wire.ConcreteType{&VoteSetBitsMessage{}, msgTypeVoteSetBits},
	wire.ConcreteType{&Maj23VotesAggrMessage{}, msgTypeMaj23VotesAggr},
	wire.ConcreteType{&VotesAggrPartMessage{}, msgTypeVotesAggrPart},
	wire.ConcreteType{&Maj23SignAggrMessage{}, msgTypeMaj23SignAggr},
	//new liaoyd
	wire.ConcreteType{&TestMessage{}, msgTypeTest},
)

// TODO: check for unnecessary extra bytes at the end.
func DecodeMessage(bz []byte) (msgType byte, msg ConsensusMessage, err error) {
	msgType = bz[0]
	n := new(int)
	r := bytes.NewReader(bz)
	msg = wire.ReadBinary(struct{ ConsensusMessage }{}, r, maxConsensusMessageSize, n, &err).(struct{ ConsensusMessage }).ConsensusMessage
	return
}

//-------------------------------------

// For every height/round/step transition
type NewRoundStepMessage struct {
	Height                int
	Round                 int
	Step                  RoundStepType
	SecondsSinceStartTime int
	LastCommitRound       int
}

//new liaoyd
// type TestMessage struct {
// 	IP    string
// 	Value string
// }
type TestMessage struct {
	ValidatorMsg *types.ValidatorMsg
}

func (m *NewRoundStepMessage) String() string {
	return fmt.Sprintf("[NewRoundStep H:%v R:%v S:%v LCR:%v]",
		m.Height, m.Round, m.Step, m.LastCommitRound)
}

//new liaoyd
func (m *TestMessage) String() string {
	return fmt.Sprintf("[TestMessage %v]", m.ValidatorMsg)
}

//-------------------------------------

type CommitStepMessage struct {
	Height           int
	BlockPartsHeader types.PartSetHeader
	BlockParts       *BitArray
}

func (m *CommitStepMessage) String() string {
	return fmt.Sprintf("[CommitStep H:%v BP:%v BA:%v]", m.Height, m.BlockPartsHeader, m.BlockParts)
}

//-------------------------------------

type ProposalMessage struct {
	Proposal *types.Proposal
}

func (m *ProposalMessage) String() string {
	return fmt.Sprintf("[Proposal %v]", m.Proposal)
}

//-------------------------------------

type ProposalPOLMessage struct {
	Height           int
	ProposalPOLRound int
	ProposalPOL      *BitArray
}

func (m *ProposalPOLMessage) String() string {
	return fmt.Sprintf("[ProposalPOL H:%v POLR:%v POL:%v]", m.Height, m.ProposalPOLRound, m.ProposalPOL)
}

//-------------------------------------

type BlockPartMessage struct {
	Height int
	Round  int
	Part   *types.Part
}

func (m *BlockPartMessage) String() string {
	return fmt.Sprintf("[BlockPart H:%v R:%v P:%v]", m.Height, m.Round, m.Part)
}

//-------------------------------------

type Maj23VotesAggrMessage struct {
	Maj23VotesAggr	*types.VotesAggr
}

func (m *Maj23VotesAggrMessage) String() string {
	return fmt.Sprintf("[Maj23VotesAggr %v]", m.Maj23VotesAggr)
}

type VotesAggrPartMessage struct {
	Height int
	Round  int
	Type   byte
	Part   *types.Part
}

func (m *VotesAggrPartMessage) String() string {
	return fmt.Sprintf("[VotesAggrPart H:%v R:%v T:%v P:%v]", m.Height, m.Round, m.Type, m.Part)
}

//-------------------------------------

type VoteMessage struct {
	Vote *types.Vote
}

func (m *VoteMessage) String() string {
	return fmt.Sprintf("[Vote %v]", m.Vote)
}

//-------------------------------------

type Maj23SignAggrMessage struct {
	Maj23SignAggr	*types.SignAggr
}

func (m *Maj23SignAggrMessage) String() string {
	return fmt.Sprintf("[Maj23VotesAggr %v]", m.Maj23SignAggr)
}

//-------------------------------------

type HasVoteMessage struct {
	Height int
	Round  int
	Type   byte
	Index  int
}

func (m *HasVoteMessage) String() string {
	return fmt.Sprintf("[HasVote VI:%v V:{%v/%02d/%v} VI:%v]", m.Index, m.Height, m.Round, m.Type, m.Index)
}

//-------------------------------------

type VoteSetMaj23Message struct {
	Height  int
	Round   int
	Type    byte
	BlockID types.BlockID
}

func (m *VoteSetMaj23Message) String() string {
	return fmt.Sprintf("[VSM23 %v/%02d/%v %v]", m.Height, m.Round, m.Type, m.BlockID)
}

//-------------------------------------

type VoteSetBitsMessage struct {
	Height  int
	Round   int
	Type    byte
	BlockID types.BlockID
	Votes   *BitArray
}

func (m *VoteSetBitsMessage) String() string {
	return fmt.Sprintf("[VSB %v/%02d/%v %v %v]", m.Height, m.Round, m.Type, m.BlockID, m.Votes)
}

//----------------------------------------------
//author@liaoyd
// type AcceptVotes struct {
// 	Height int            `json:"height"`
// 	Key    string         `json:"key"`
// 	PubKey crypto.PubKey  `json:"pub_key"`
// 	Power  uint64         `"power"`
// 	Sum    int64          `"sum"`
// 	Votes  []*types.ExMsg `votes`
// 	Maj23  bool           `"maj23"`
// }

// type PreVal struct {
// 	ValidatorSet *types.ValidatorSet `json:"validator_set"`
// }

// var AcceptVoteSet map[string]*types.AcceptVotes //votes

func (conR *ConsensusReactor) NewAcceptVotes(msg *types.ValidatorMsg, key string) *types.AcceptVotes {
	return &types.AcceptVotes{
		Epoch: msg.Epoch,
		Key: key,
		PubKey: msg.PubKey,
		Power: msg.Power,
		Action: msg.Action,
		Sum:   0,
		Votes: make([]*types.ValidatorMsg, conR.conS.Validators.Size()),
		Maj23: false,
	}
}

//------------------------------------------
var ValidatorMsgList = clist.New() //transfer

var ValidatorMsgMap map[string]*types.ValidatorMsg //request

func SendValidatorMsgToCons(from string, key string, epoch int, power uint64, action string, target string) {
	// fmt.Println("in func SendExMsgToCons(s string)")
	validatorMsg := types.NewValidatorMsg(from, key, epoch, power, action, target)

	ValidatorMsgList.PushBack(validatorMsg)
}

func (conR *ConsensusReactor) validatorExMsgRoutine(peer *p2p.Peer, ps *PeerState) {
	fmt.Println("in func validatorExMsgRoutine(peer *p2p.Peer, ps *PeerState)")
	types.AcceptVoteSet = make(map[string]*types.AcceptVotes)
	// AcceptVoteSet := types.AcceptVoteSet
	ValidatorMsgMap = make(map[string]*types.ValidatorMsg)
	// AcceptVoteSet = make(map[string]*types.AcceptVotes)

	var next *clist.CElement

	for {
		if next == nil {
			next = ValidatorMsgList.FrontWait()
		}
		msg := next.Value.(*types.ValidatorMsg)
		fmt.Println("msg:", msg)

		switch msg.Action {
		case "JOIN": //joinValidator
			// privVal := types.LoadPrivValidator("/mnt/vdb/ethermint-validatortest/.ethermint/priv_validator.json")
			privVal := types.LoadPrivValidator(conR.conS.config.GetString("priv_validator_file"))
			msg.PubKey = privVal.PubKey
			// fmt.Println("priVal:", privVal)
			// fmt.Println("priValPubKey:", privVal.PubKey)
			tMsg := &TestMessage{ValidatorMsg: msg}
			fmt.Println("broadcast message!!!!!!")
			peer.Send(StateChannel, struct{ ConsensusMessage }{tMsg})

			from := msg.From
			ValidatorMsgMap[from] = msg
			if _, ok := types.AcceptVoteSet[from]; !ok {
				types.AcceptVoteSet[from] = conR.NewAcceptVotes(msg, from) //new vote list to add vote
			}
		case "ACCEPT": //acceptJoinReq
			if conR.conS.privValidator == nil || !conR.conS.Validators.HasAddress(conR.conS.privValidator.GetAddress()) {
				fmt.Println("we are not in validator set")
				break
			}
			if received, ok := ValidatorMsgMap[msg.Target]; ok {
				if received.Epoch <= conR.conS.Epoch.Number {
					fmt.Println("request height is lower than consensus height")
					break
				}
				if received.Power == msg.Power && received.Epoch == msg.Epoch {
					addr := conR.conS.privValidator.GetAddress()
					valIndex, _ := conR.conS.Validators.GetByAddress(addr)
					msg.ValidatorIndex = valIndex
					msg.PubKey = received.PubKey

					conR.conS.privValidator.SignValidatorMsg(conR.conS.state.ChainID, msg)
					tMsg := &TestMessage{ValidatorMsg: msg}
					fmt.Println("sending tMsg!!!", tMsg)
					// conR.Switch.Broadcast(StateChannel, struct{ ConsensusMessage }{tMsg})
					peer.Send(StateChannel, struct{ ConsensusMessage }{tMsg})
					conR.tryAddAcceptVote(msg)

				} else {
					fmt.Println("different power or height")
				}
			} else {
				fmt.Println("didn't received JOIN request")
			}
		}
		next = next.NextWait()
		continue
	}
}

func (conR *ConsensusReactor) tryAddAcceptVote(validatorMsg *types.ValidatorMsg) (success bool, err error) {
	fmt.Println("in func (conR *ConsensusReactor) tryAddAcceptVote(validatorMsg *types.ValidatorMsg) (success bool, err error)")
	_, val := conR.conS.Validators.GetByIndex(validatorMsg.ValidatorIndex)
	if val == nil {
		fmt.Println("bad index!!!!!")
		return false, types.ErrVoteInvalidValidatorIndex
	}
	if !val.PubKey.VerifyBytes(types.SignBytes(conR.conS.state.ChainID, validatorMsg), validatorMsg.Signature) {
		// Bad signature.
		fmt.Println("bad signature!!!!!")
		return false, types.ErrVoteInvalidSignature
	}
	if validatorMsg.Epoch <= conR.conS.Epoch.Number {
		fmt.Println("bad height!!!!!")
		return false, nil
	}
	return conR.addAcceptVotes(validatorMsg)
}

func (conR *ConsensusReactor) addAcceptVotes(validatorMsg *types.ValidatorMsg) (success bool, err error) {
	fmt.Println("in func (conR *ConsensusReactor) addAcceptVotes(validatorMsg *types.ValidatorMsg) (success bool, err error)")
	_, val := conR.conS.Validators.GetByIndex(validatorMsg.ValidatorIndex)
	target := validatorMsg.Target
	if types.AcceptVoteSet[target].Votes[validatorMsg.ValidatorIndex] != nil {
		fmt.Println("duplicate vote!!!")
		return false, nil
	}
	types.AcceptVoteSet[target].Sum += val.VotingPower
	types.AcceptVoteSet[target].Votes[validatorMsg.ValidatorIndex] = validatorMsg
	if types.AcceptVoteSet[target].Sum > conR.conS.Validators.TotalVotingPower()*2/3 && !types.AcceptVoteSet[target].Maj23 {
		fmt.Println("update validator set!!!!")
		types.AcceptVoteSet[target].Maj23 = true
		//these following values should be filled when join/withdraw, and not modified here
		/*
		types.AcceptVoteSet[target].Epoch = validatorMsg.Epoch
		types.AcceptVoteSet[target].PubKey = validatorMsg.PubKey
		types.AcceptVoteSet[target].Power = validatorMsg.Power
		types.AcceptVoteSet[target].Key = target
		types.AcceptVoteSet[target].Action = validatorMsg.Action
		*/
	}
	return true, nil
}

func (conR *ConsensusReactor) GetDiffValidator() {
	types.ValidatorChannel = make(chan int)
	types.EndChannel = make(chan []*abci.Validator)
	val, err := OpenVAL(conR.conS.config.GetString("cs_val_file"))
	// AcceptVoteSet := types.AcceptVoteSet
	if err != nil {
		fmt.Println("ERROR IN OPENVAL", err)
	}
	for {
		var diffs []*abci.Validator
		init := 0
		epochNumber := <-types.ValidatorChannel
		for k, v := range types.AcceptVoteSet {
			if v.Epoch == epochNumber {
				fmt.Println("k:", k, "v:", v)
				if v.Maj23 {
					diffs = append(
						diffs,
						&abci.Validator{
							PubKey: v.PubKey.Bytes(),
							Power:  v.Power,
						},
					)
					if init == 0 {
						val.writeEpoch(v.Epoch)
						val.Save(&types.PreVal{ValidatorSet: conR.conS.Validators})
						init = 1
					}
					val.Save(v)

					types.ValChangedEpoch[v.Epoch] = append(
						types.ValChangedEpoch[v.Epoch],
						v,
					)
				}
				//delete(ValidatorMsgMap, v.Key)
				//delete(types.AcceptVoteSet, v.Key)
				delete(ValidatorMsgMap, k)
				delete(types.AcceptVoteSet, k)
			}
		}
		types.EndChannel <- diffs
	}
}
