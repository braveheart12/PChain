package types

import (
	// for registering TMEventData as events.EventData
	abci "github.com/tendermint/abci/types"
	. "github.com/tendermint/go-common"
	"github.com/tendermint/go-events"
	"github.com/tendermint/go-wire"
)

// Functions to generate eventId strings

// Reserved
func EventStringBond() string    { return "Bond" }
func EventStringUnbond() string  { return "Unbond" }
func EventStringRebond() string  { return "Rebond" }
func EventStringDupeout() string { return "Dupeout" }
func EventStringFork() string    { return "Fork" }
func EventStringTx(tx Tx) string { return Fmt("Tx:%X", tx.Hash()) }

func EventStringNewBlock() string         { return "NewBlock" }
func EventStringNewBlockHeader() string   { return "NewBlockHeader" }
func EventStringNewRound() string         { return "NewRound" }
func EventStringNewRoundStep() string     { return "NewRoundStep" }
func EventStringTimeoutPropose() string   { return "TimeoutPropose" }
func EventStringCompleteProposal() string { return "CompleteProposal" }
func EventStringPolka() string            { return "Polka" }
func EventStringUnlock() string           { return "Unlock" }
func EventStringLock() string             { return "Lock" }
func EventStringRelock() string           { return "Relock" }
func EventStringTimeoutWait() string      { return "TimeoutWait" }
func EventStringVote() string             { return "Vote" }
func EventStringSignAggr() string 		   { return  "SignAggr"}
func EventStringVote2Proposer() string    { return "Vote2Proposer"}
func EventStringProposal() string         { return "Proposal"}
func EventStringBlockPart() string        { return "BlockPart"}
func EventStringProposalBlockParts() string { return "Proposal_BlockParts"}
func EventStringSwitchToFastSync() string  { return "Switch_FastSync"}

//----------------------------------------

// implements events.EventData
type TMEventData interface {
	events.EventData
	AssertIsTMEventData()
}

const (
	EventDataTypeNewBlock       = byte(0x01)
	EventDataTypeFork           = byte(0x02)
	EventDataTypeTx             = byte(0x03)
	EventDataTypeNewBlockHeader = byte(0x04)

	EventDataTypeRoundState = byte(0x11)
	EventDataTypeVote       = byte(0x12)
	EventDataTypeSignAggr       = byte(0x13)
	EventDataTypeVote2Proposer = byte(0x14)
	EventDataTypeProposal      = byte(0x15)
	EventDataTypeBlockPart    = byte(0x16)
	EventDataTypeProposalBlockParts = byte(0x17)
	EventDataTypeSwitchToFastSync = byte(0x18)
)

var _ = wire.RegisterInterface(
	struct{ TMEventData }{},
	wire.ConcreteType{EventDataNewBlock{}, EventDataTypeNewBlock},
	wire.ConcreteType{EventDataNewBlockHeader{}, EventDataTypeNewBlockHeader},
	// wire.ConcreteType{EventDataFork{}, EventDataTypeFork },
	wire.ConcreteType{EventDataTx{}, EventDataTypeTx},
	wire.ConcreteType{EventDataRoundState{}, EventDataTypeRoundState},
	wire.ConcreteType{EventDataVote{}, EventDataTypeVote},
	wire.ConcreteType{EventDataSignAggr{}, EventDataTypeSignAggr},
	wire.ConcreteType{EventDataVote2Proposer{}, EventDataTypeVote2Proposer},
	wire.ConcreteType{EventDataProposal{}, EventDataTypeProposal},
	wire.ConcreteType{EventDataBlockPart{}, EventDataTypeBlockPart},
	wire.ConcreteType{EventDataProposalBlockParts{}, EventDataTypeProposalBlockParts},
	wire.ConcreteType{EventDataSwitchToFastSync{}, EventDataTypeSwitchToFastSync},
)

// Most event messages are basic types (a block, a transaction)
// but some (an input to a call tx or a receive) are more exotic

type EventDataNewBlock struct {
	Block *Block `json:"block"`
}

// light weight event for benchmarking
type EventDataNewBlockHeader struct {
	Header *Header `json:"header"`
}

// All txs fire EventDataTx
type EventDataTx struct {
	Height int           `json:"height"`
	Tx     Tx            `json:"tx"`
	Data   []byte        `json:"data"`
	Log    string        `json:"log"`
	Code   abci.CodeType `json:"code"`
	Error  string        `json:"error"` // this is redundant information for now
}

// NOTE: This goes into the replay WAL
type EventDataRoundState struct {
	Height int    `json:"height"`
	Round  int    `json:"round"`
	Step   string `json:"step"`

	// private, not exposed to websockets
	RoundState interface{} `json:"-"`
}

type EventDataVote struct {
	Vote *Vote
}

type EventDataSignAggr struct {
	SignAggr *SignAggr
}

type EventDataVote2Proposer struct {
	Vote *Vote
	ProposerKey string
}

type EventDataProposal struct {
	Proposal *Proposal
}

type EventDataBlockPart struct {
	Round int
	Height int
	Part *Part
}

type EventDataProposalBlockParts struct {
	Proposal *Proposal
	Parts *PartSet
}

type EventDataSwitchToFastSync struct {}

func (_ EventDataNewBlock) AssertIsTMEventData()       {}
func (_ EventDataNewBlockHeader) AssertIsTMEventData() {}
func (_ EventDataTx) AssertIsTMEventData()             {}
func (_ EventDataRoundState) AssertIsTMEventData()     {}
func (_ EventDataVote) AssertIsTMEventData()           {}
func (_ EventDataSignAggr) AssertIsTMEventData()		{}
func (_ EventDataVote2Proposer) AssertIsTMEventData()	{}
func (_ EventDataProposal) AssertIsTMEventData()		{}
func (_ EventDataBlockPart) AssertIsTMEventData() 		{}
func (_ EventDataProposalBlockParts) AssertIsTMEventData() {}
func (_ EventDataSwitchToFastSync) AssertIsTMEventData() {}
//----------------------------------------
// Wrappers for type safety

type Fireable interface {
	events.Fireable
}

type Eventable interface {
	SetEventSwitch(EventSwitch)
}

type EventSwitch interface {
	events.EventSwitch
}

type EventCache interface {
	Fireable
	Flush()
}

func NewEventSwitch() EventSwitch {
	return events.NewEventSwitch()
}

func NewEventCache(evsw EventSwitch) EventCache {
	return events.NewEventCache(evsw)
}

// All events should be based on this FireEvent to ensure they are TMEventData
func fireEvent(fireable events.Fireable, event string, data TMEventData) {
	if fireable != nil {
		fireable.FireEvent(event, data)
	}
}

func AddListenerForEvent(evsw EventSwitch, id, event string, cb func(data TMEventData)) {
	evsw.AddListenerForEvent(id, event, func(data events.EventData) {
		cb(data.(TMEventData))
	})

}

//--- block, tx, and vote events

func FireEventNewBlock(fireable events.Fireable, block EventDataNewBlock) {
	fireEvent(fireable, EventStringNewBlock(), block)
}

func FireEventNewBlockHeader(fireable events.Fireable, header EventDataNewBlockHeader) {
	fireEvent(fireable, EventStringNewBlockHeader(), header)
}

func FireEventVote(fireable events.Fireable, vote EventDataVote) {
	fireEvent(fireable, EventStringVote(), vote)
}

func FireEventTx(fireable events.Fireable, tx EventDataTx) {
	fireEvent(fireable, EventStringTx(tx.Tx), tx)
}

func FireEventSignAggr(fireable events.Fireable, sign EventDataSignAggr) {
	fireEvent(fireable, EventStringSignAggr(), sign)
}

func FireEventVote2Proposer(fireable events.Fireable, vote EventDataVote2Proposer) {
	fireEvent(fireable, EventStringVote2Proposer(), vote)
}

func FireEventProposal(fireable events.Fireable, proposal EventDataProposal) {
	fireEvent(fireable, EventStringProposal(), proposal)
}

func FireEventBlockPart(fireable events.Fireable, blockPart EventDataBlockPart) {
	fireEvent(fireable, EventStringBlockPart(), blockPart)
}

func FireEventProposalBlockParts(fireable events.Fireable, propsal_blockParts EventDataProposalBlockParts) {
	fireEvent(fireable, EventStringProposalBlockParts(), propsal_blockParts)
}

//--- EventDataRoundState events

func FireEventNewRoundStep(fireable events.Fireable, rs EventDataRoundState) {
	fireEvent(fireable, EventStringNewRoundStep(), rs)
}

func FireEventTimeoutPropose(fireable events.Fireable, rs EventDataRoundState) {
	fireEvent(fireable, EventStringTimeoutPropose(), rs)
}

func FireEventTimeoutWait(fireable events.Fireable, rs EventDataRoundState) {
	fireEvent(fireable, EventStringTimeoutWait(), rs)
}

func FireEventNewRound(fireable events.Fireable, rs EventDataRoundState) {
	fireEvent(fireable, EventStringNewRound(), rs)
}

func FireEventCompleteProposal(fireable events.Fireable, rs EventDataRoundState) {
	fireEvent(fireable, EventStringCompleteProposal(), rs)
}

func FireEventPolka(fireable events.Fireable, rs EventDataRoundState) {
	fireEvent(fireable, EventStringPolka(), rs)
}

func FireEventUnlock(fireable events.Fireable, rs EventDataRoundState) {
	fireEvent(fireable, EventStringUnlock(), rs)
}

func FireEventRelock(fireable events.Fireable, rs EventDataRoundState) {
	fireEvent(fireable, EventStringRelock(), rs)
}

func FireEventLock(fireable events.Fireable, rs EventDataRoundState) {
	fireEvent(fireable, EventStringLock(), rs)
}

//--- EventSwitch events
func FireEventSwitchToFastSync(fireable events.Fireable, switch2sync EventDataSwitchToFastSync) {
	fireEvent(fireable, EventStringSwitchToFastSync(), switch2sync)
}
