package tendermint

import (
	"bytes"
	"errors"
	abci "github.com/tendermint/abci/types"
	cmn "github.com/tendermint/go-common"
	cfg "github.com/tendermint/go-config"
	"github.com/tendermint/go-crypto"
	dbm "github.com/tendermint/go-db"
	"github.com/tendermint/go-p2p"
	"net"
	"net/http"
	"strings"

	bc "github.com/tendermint/tendermint/blockchain"
	"github.com/tendermint/tendermint/consensus"
	mempl "github.com/tendermint/tendermint/mempool"
	"github.com/tendermint/tendermint/proxy"
	rpccore "github.com/tendermint/tendermint/rpc/core"
	grpccore "github.com/tendermint/tendermint/rpc/grpc"
	sm "github.com/tendermint/tendermint/state"
	"github.com/tendermint/tendermint/state/txindex"
	"github.com/tendermint/tendermint/state/txindex/kv"
	"github.com/tendermint/tendermint/state/txindex/null"
	"github.com/tendermint/tendermint/types"

	"fmt"
	_ "net/http/pprof"
	//"github.com/pchain/ethermint/utils"
	"github.com/pchain/common/plogger"
	"github.com/pchain/ethermint/app"
	"github.com/tendermint/go-rpc/server"
	ep "github.com/tendermint/tendermint/epoch"
	rpcTxHook "github.com/tendermint/tendermint/rpc/core/txhook"
	"io/ioutil"
	"os"
	"time"
)

var logger = plogger.GetLogger("node")

type Node struct {
	cmn.BaseService
	// config
	config        cfg.Config           // user config
	genesisDoc    *types.GenesisDoc    // initial validator set
	privValidator *types.PrivValidator // local node's validator key

	stateDB dbm.DB
	epochDB dbm.DB

	sw       *p2p.Switch   // p2p connections
	addrBook *p2p.AddrBook // known peers

	// services
	evsw             types.EventSwitch           // pub/sub for services
	blockStore       *bc.BlockStore              // store the blockchain to disk
	bcReactor        *bc.BlockchainReactor       // for fast-syncing
	mempoolReactor   *mempl.MempoolReactor       // for gossipping transactions
	consensusState   *consensus.ConsensusState   // latest consensus state
	consensusReactor *consensus.ConsensusReactor // for participating in the consensus
	proxyApp         proxy.AppConns              // connection to the application
	rpcListeners     []net.Listener              // rpc servers
	txIndexer        txindex.TxIndexer

	cch rpcTxHook.CrossChainHelper
}

// Deprecated
func NewNodeDefault(config cfg.Config, cch rpcTxHook.CrossChainHelper) *Node {
	// Get PrivValidator
	privValidatorFile := config.GetString("priv_validator_file")
	keydir := config.GetString("keystore")
	privValidator := types.LoadOrGenPrivValidator(privValidatorFile, keydir)
	return NewNode(config, privValidator, proxy.DefaultClientCreator(config), cch)
}

// Deprecated
func NewNode(config cfg.Config, privValidator *types.PrivValidator,
	clientCreator proxy.ClientCreator, cch rpcTxHook.CrossChainHelper) *Node {

	// Get BlockStore
	blockStoreDB := dbm.NewDB("blockstore", config.GetString("db_backend"), config.GetString("db_dir"))
	blockStore := bc.NewBlockStore(blockStoreDB)

	ep.VADB = dbm.NewDB("validatoraction", config.GetString("db_backend"), config.GetString("db_dir"))

	// Get State And Epoch
	stateDB := dbm.NewDB("state", config.GetString("db_backend"), config.GetString("db_dir"))
	epochDB := dbm.NewDB("epoch", config.GetString("db_backend"), config.GetString("db_dir"))
	state, epoch := InitStateAndEpoch(config, stateDB, epochDB)

	// add the chainid and number of validators to the global config
	config.Set("chain_id", state.ChainID)
	config.Set("num_vals", state.Epoch.Validators.Size())

	// Create the proxyApp, which manages connections (consensus, mempool, query)
	// and sync tendermint and the app by replaying any necessary blocks
	proxyApp := proxy.NewAppConns(config, clientCreator, consensus.NewHandshaker(config, state, blockStore, cch))
	if _, err := proxyApp.Start(); err != nil {
		cmn.Exit(cmn.Fmt("Error starting proxy app connections: %v", err))
	}

	// reload the state (it may have been updated by the handshake)
	state = sm.LoadState(stateDB)
	epoch = ep.LoadOneEpoch(epochDB, state.LastEpochNumber)
	state.Epoch = epoch

	//_, _ = consensus.OpenVAL(config.GetString("cs_val_file")) //load validator change from val
	fmt.Println("state.Validators:", state.Epoch.Validators)

	// Transaction indexing
	var txIndexer txindex.TxIndexer
	switch config.GetString("tx_index") {
	case "kv":
		store := dbm.NewDB("tx_index", config.GetString("db_backend"), config.GetString("db_dir"))
		txIndexer = kv.NewTxIndex(store)
	default:
		txIndexer = &null.TxIndex{}
	}
	state.TxIndexer = txIndexer

	// Generate node PrivKey
	//privKey := crypto.GenPrivKeyEd25519()

	// Make event switch
	eventSwitch := types.NewEventSwitch()
	_, err := eventSwitch.Start()
	if err != nil {
		cmn.Exit(cmn.Fmt("Failed to start switch: %v", err))
	}

	// Decide whether to fast-sync or not
	// We don't fast-sync when the only validator is us.
	fastSync := config.GetBool("fast_sync")
	if state.Epoch.Validators.Size() == 1 {
		addr, _ := state.Epoch.Validators.GetByIndex(0)
		if bytes.Equal(privValidator.Address, addr) {
			fastSync = false
		}
	}

	// Make BlockchainReactor
	bcReactor := bc.NewBlockchainReactor(config, state.Copy(), proxyApp.Consensus(), blockStore, fastSync, cch)

	// Make MempoolReactor
	mempool := mempl.NewMempool(config, proxyApp.Mempool())
	mempoolReactor := mempl.NewMempoolReactor(config, mempool, state.ChainID)

	// Make ConsensusReactor
	consensusState := consensus.NewConsensusState(config, state.Copy(), proxyApp.Consensus(), blockStore, mempool, epoch, cch)
	if privValidator != nil {
		consensusState.SetPrivValidator(privValidator)
	}
	consensusReactor := consensus.NewConsensusReactor(consensusState, fastSync)

	// Make p2p network switch
	sw := p2p.NewSwitch(config.GetConfig("p2p"))
	sw.AddReactor(state.ChainID, "MEMPOOL", mempoolReactor)
	sw.AddReactor(state.ChainID, "BLOCKCHAIN", bcReactor)
	sw.AddReactor(state.ChainID, "CONSENSUS", consensusReactor)

	// Optionally, start the pex reactor
	var addrBook *p2p.AddrBook
	if config.GetBool("pex_reactor") {
		addrBook = p2p.NewAddrBook(config.GetString("addrbook_file"), config.GetBool("addrbook_strict"))
		pexReactor := p2p.NewPEXReactor(addrBook)
		sw.AddReactor(state.ChainID, "PEX", pexReactor)
	}

	// Filter peers by addr or pubkey with an ABCI query.
	// If the query return code is OK, add peer.
	// XXX: Query format subject to change
	if config.GetBool("filter_peers") {
		// NOTE: addr is ip:port
		sw.SetAddrFilter(func(addr net.Addr) error {
			resQuery, err := proxyApp.Query().QuerySync(abci.RequestQuery{Path: cmn.Fmt("/p2p/filter/addr/%s", addr.String())})
			if err != nil {
				return err
			}
			if resQuery.Code.IsOK() {
				return nil
			}
			return errors.New(resQuery.Code.String())
		})
		sw.SetPubKeyFilter(func(pubkey crypto.PubKeyEd25519) error {
			resQuery, err := proxyApp.Query().QuerySync(abci.RequestQuery{Path: cmn.Fmt("/p2p/filter/pubkey/%X", pubkey.Bytes())})
			if err != nil {
				return err
			}
			if resQuery.Code.IsOK() {
				return nil
			}
			return errors.New(resQuery.Code.String())
		})
	}

	// add the event switch to all services
	// they should all satisfy events.Eventable
	SetEventSwitch(eventSwitch, bcReactor, mempoolReactor, consensusReactor)

	// run the profile server
	profileHost := config.GetString("prof_laddr")
	if profileHost != "" {

		go func() {
			logger.Warn("Profile server", " error:", http.ListenAndServe(profileHost, nil))
		}()
	}

	node := &Node{
		config:        config,
		genesisDoc:    state.GenesisDoc,
		privValidator: privValidator,

		stateDB: stateDB,
		epochDB: epochDB,
		//privKey:  privKey,
		//sw:       sw,
		//addrBook: addrBook,

		evsw:             eventSwitch,
		blockStore:       blockStore,
		bcReactor:        bcReactor,
		mempoolReactor:   mempoolReactor,
		consensusState:   consensusState,
		consensusReactor: consensusReactor,
		proxyApp:         proxyApp,
		txIndexer:        txIndexer,

		cch: cch,
	}
	node.BaseService = *cmn.NewBaseService(logger, "Node", node)
	return node
}

func NewNodeNotStart(config cfg.Config, sw *p2p.Switch, addrBook *p2p.AddrBook, cl *rpcserver.ChannelListener,
	cch rpcTxHook.CrossChainHelper) *Node {
	// Get PrivValidator
	privValidatorFile := config.GetString("priv_validator_file")
	keydir := config.GetString("keystore")
	privValidator := types.LoadOrGenPrivValidator(privValidatorFile, keydir)
	// ClientCreator will be instantiated later after ethermint proxyapp created
	// clientCreator := proxy.DefaultClientCreator(config)

	// Get BlockStore
	blockStoreDB := dbm.NewDB("blockstore", config.GetString("db_backend"), config.GetString("db_dir"))
	blockStore := bc.NewBlockStore(blockStoreDB)

	ep.VADB = dbm.NewDB("validatoraction", config.GetString("db_backend"), config.GetString("db_dir"))

	// Get State And Epoch
	stateDB := dbm.NewDB("state", config.GetString("db_backend"), config.GetString("db_dir"))
	epochDB := dbm.NewDB("epoch", config.GetString("db_backend"), config.GetString("db_dir"))
	state, _ := InitStateAndEpoch(config, stateDB, epochDB)

	// add the chainid and number of validators to the global config
	// TODO There is No Global Config, to be removed
	config.Set("chain_id", state.ChainID)
	config.Set("num_vals", state.Epoch.Validators.Size())

	// Create the proxyApp, which manages connections (consensus, mempool, query)
	// and sync tendermint and the app by replaying any necessary blocks
	proxyApp := proxy.NewAppConns(config, nil, consensus.NewHandshaker(config, state, blockStore, cch))

	// Make event switch
	eventSwitch := types.NewEventSwitch()

	// Initial the RPC Listeners, the 1st one must be channel listener
	rpcListeners := make([]net.Listener, 1)
	rpcListeners[0] = cl

	node := &Node{
		config:        config,
		genesisDoc:    state.GenesisDoc,
		privValidator: privValidator,

		stateDB: stateDB,
		epochDB: epochDB,

		sw:       sw,
		addrBook: addrBook,

		evsw:       eventSwitch,
		blockStore: blockStore,
		proxyApp:   proxyApp,

		rpcListeners: rpcListeners,

		cch: cch,
	}
	node.BaseService = *cmn.NewBaseService(logger, "Node", node)
	return node
}

func (n *Node) OnStart1() error {

	if _, err := n.proxyApp.Start(); err != nil {
		cmn.Exit(cmn.Fmt("Error starting proxy app connections: %v", err))
	}

	// reload the state (it may have been updated by the handshake)
	state := sm.LoadState(n.stateDB)
	epoch := ep.LoadOneEpoch(n.epochDB, state.LastEpochNumber)
	state.Epoch = epoch

	//_, _ = consensus.OpenVAL(config.GetString("cs_val_file")) //load validator change from val
	fmt.Println("state.Validators:", state.Epoch.Validators)

	// Transaction indexing
	var txIndexer txindex.TxIndexer
	switch n.config.GetString("tx_index") {
	case "kv":
		store := dbm.NewDB("tx_index", n.config.GetString("db_backend"), n.config.GetString("db_dir"))
		txIndexer = kv.NewTxIndex(store)
	default:
		txIndexer = &null.TxIndex{}
	}
	state.TxIndexer = txIndexer

	_, err := n.evsw.Start()
	if err != nil {
		cmn.Exit(cmn.Fmt("Failed to start switch: %v", err))
	}

	// Decide whether to fast-sync or not
	// We don't fast-sync when the only validator is us.
	fastSync := n.config.GetBool("fast_sync")
	if state.Epoch.Validators.Size() == 1 {
		addr, _ := state.Epoch.Validators.GetByIndex(0)
		if bytes.Equal(n.privValidator.Address, addr) {
			fastSync = false
		}
	}

	// Make BlockchainReactor
	bcReactor := bc.NewBlockchainReactor(n.config, state.Copy(), n.proxyApp.Consensus(),
		n.blockStore, fastSync, n.cch)

	// Make MempoolReactor
	mempool := mempl.NewMempool(n.config, n.proxyApp.Mempool())
	mempool.SetEpoch(epoch)
	mempoolReactor := mempl.NewMempoolReactor(n.config, mempool, state.ChainID)

	// Make ConsensusReactor
	consensusState := consensus.NewConsensusState(n.config, state.Copy(), n.proxyApp.Consensus(),
		n.blockStore, mempool, epoch, n.cch)
	if n.privValidator != nil {
		consensusState.SetPrivValidator(n.privValidator)
	}

	//TODO: The node info may not be ready???
	consensusState.SetNodeInfo(n.sw.NodeInfo())
	consensusReactor := consensus.NewConsensusReactor(consensusState, fastSync)

	// Add Reactor to P2P Switch
	n.sw.AddReactor(state.ChainID, "MEMPOOL", mempoolReactor)
	n.sw.AddReactor(state.ChainID, "BLOCKCHAIN", bcReactor)
	n.sw.AddReactor(state.ChainID, "CONSENSUS", consensusReactor)

	// add the event switch to all services
	// they should all satisfy events.Eventable
	SetEventSwitch(n.evsw, bcReactor, mempoolReactor, consensusReactor)

	// run the profile server
	profileHost := n.config.GetString("prof_laddr")
	if profileHost != "" {

		go func() {
			logger.Warn("Profile server", "error", http.ListenAndServe(profileHost, nil))
		}()
	}

	// Add ChainID to P2P Node Info
	n.sw.NodeInfo().AddNetwork(state.ChainID)

	// Start the Reactors for this Chain
	n.sw.StartChainReactor(state.ChainID)

	n.bcReactor = bcReactor
	n.mempoolReactor = mempoolReactor
	n.consensusState = consensusState
	n.consensusReactor = consensusReactor
	n.txIndexer = txIndexer

	// Run the RPC server
	//if n.config.GetString("rpc_laddr") != "" {
	_, err = n.StartRPC()
	if err != nil {
		return err
	}
	//n.rpcListeners = listeners
	//}

	return nil
}

// Deprecated
func (n *Node) OnStart() error {

	fmt.Printf("(n *Node) OnStart()")

	// Create & add listener
	//protocol, address := ProtocolAndAddress(n.config.GetString("node_laddr"))
	//l := p2p.NewDefaultListener(protocol, address, n.config.GetBool("skip_upnp"))
	//n.sw.AddListener(l)
	//
	//// Start the switch
	//n.sw.SetNodeInfo(n.makeNodeInfo())
	//n.sw.SetNodePrivKey(n.privKey)
	//_, err := n.sw.Start()
	//if err != nil {
	//	return err
	//}
	//
	//// If seeds exist, add them to the address book and dial out
	//if n.config.GetString("seeds") != "" {
	//	// dial out
	//	seeds := strings.Split(n.config.GetString("seeds"), ",")
	//	if err := n.DialSeeds(seeds); err != nil {
	//		return err
	//	}
	//}
	//
	//// Run the RPC server
	//if n.config.GetString("rpc_laddr") != "" {
	//	listeners, err := n.StartRPC()
	//	if err != nil {
	//		return err
	//	}
	//	n.rpcListeners = listeners
	//}

	return nil
}

func (n *Node) OnStop() {
	n.BaseService.OnStop()

	for _, l := range n.rpcListeners {
		logger.Info("Closing rpc listener ", l)
		if err := l.Close(); err != nil {
			logger.Error("Error closing listener ", l, " error:", err)
		}
	}
}

func (n *Node) RunForever() {
	// Sleep forever and then...
	cmn.TrapSignal(func() {
		n.Stop()
	})
}

// Add the event switch to reactors, mempool, etc.
func SetEventSwitch(evsw types.EventSwitch, eventables ...types.Eventable) {
	for _, e := range eventables {
		e.SetEventSwitch(evsw)
	}
}

// Add a Listener to accept inbound peer connections.
// Add listeners before starting the Node.
// The first listener is the primary listener (in NodeInfo)
func (n *Node) AddListener(l p2p.Listener) {
	n.sw.AddListener(l)
}

// ConfigureRPC sets all variables in rpccore so they will serve
// rpc calls from this node
func (n *Node) ConfigureRPC() *rpccore.RPCDataContext {
	rpcData := &rpccore.RPCDataContext{}

	rpcData.SetConfig(n.config)
	rpcData.SetEventSwitch(n.evsw)
	rpcData.SetBlockStore(n.blockStore)
	rpcData.SetConsensusState(n.consensusState)
	rpcData.SetMempool(n.mempoolReactor.Mempool)
	rpcData.SetSwitch(n.sw)
	rpcData.SetPubKey(n.privValidator.PubKey)
	rpcData.SetGenesisDoc(n.genesisDoc)
	rpcData.SetAddrBook(n.addrBook)
	rpcData.SetProxyAppQuery(n.proxyApp.Query())
	rpcData.SetTxIndexer(n.txIndexer)

	return rpcData
}

func (n *Node) StartRPC() ([]net.Listener, error) {
	rpcData := n.ConfigureRPC()

	// We are using Channel Server instead of Http/Websocket Server
	cl := n.rpcListeners[0]
	mux := http.NewServeMux()
	rpcserver.RegisterRPCFuncs(mux, rpccore.Routes, rpcData)
	rpcserver.StartChannelServer(cl, mux)

	/*
		listenAddrs := strings.Split(n.config.GetString("rpc_laddr"), ",")

		// we may expose the rpc over both a unix and tcp socket
		listeners := make([]net.Listener, len(listenAddrs))
		for i, listenAddr := range listenAddrs {
			mux := http.NewServeMux()
			wm := rpcserver.NewWebsocketManager(rpccore.Routes, n.evsw)
			mux.HandleFunc("/websocket", wm.WebsocketHandler)
			rpcserver.RegisterRPCFuncs(mux, rpccore.Routes)
			listener, err := rpcserver.StartHTTPServer(listenAddr, mux)
			if err != nil {
				return nil, err
			}
			listeners[i] = listener
		}
	*/

	// we expose a simplified api over grpc for convenience to app devs
	grpcListenAddr := n.config.GetString("grpc_laddr")
	if grpcListenAddr != "" {
		listener, err := grpccore.StartGRPCServer(grpcListenAddr)
		if err != nil {
			return nil, err
		}
		n.rpcListeners = append(n.rpcListeners, listener)
	}

	return n.rpcListeners, nil
}

func (n *Node) BlockStore() *bc.BlockStore {
	return n.blockStore
}

func (n *Node) ConsensusState() *consensus.ConsensusState {
	return n.consensusState
}

func (n *Node) ConsensusReactor() *consensus.ConsensusReactor {
	return n.consensusReactor
}

func (n *Node) MempoolReactor() *mempl.MempoolReactor {
	return n.mempoolReactor
}

func (n *Node) EventSwitch() types.EventSwitch {
	return n.evsw
}

// XXX: for convenience
func (n *Node) PrivValidator() *types.PrivValidator {
	return n.privValidator
}

func (n *Node) GenesisDoc() *types.GenesisDoc {
	return n.genesisDoc
}

func (n *Node) ProxyApp() proxy.AppConns {
	return n.proxyApp
}

func InitStateAndEpoch(config cfg.Config, stateDB dbm.DB, epochDB dbm.DB) (state *sm.State, epoch *ep.Epoch) {

	state = sm.LoadState(stateDB)
	if state == nil { //first run, generate state and epoch from genesis doc

		genDocFile := config.GetString("genesis_file")
		if !cmn.FileExists(genDocFile) {
			cmn.Exit(cmn.Fmt("InitStateAndEpoch(), Couldn't find GenesisDoc file"))
		}

		jsonBlob, err := ioutil.ReadFile(genDocFile)
		if err != nil {
			cmn.Exit(cmn.Fmt("InitStateAndEpoch(), Couldn't read GenesisDoc file: %v", err))
		}

		genDoc, err := types.GenesisDocFromJSON(jsonBlob)
		if err != nil {
			cmn.PanicSanity(cmn.Fmt("InitStateAndEpoch(), Genesis doc parse json error: %v", err))
		}

		state = sm.MakeGenesisState(stateDB, genDoc)
		state.Save()

		rewardScheme := ep.MakeRewardScheme(epochDB, &genDoc.RewardScheme)
		epoch = ep.MakeOneEpoch(epochDB, &genDoc.CurrentEpoch)
		epoch.RS = rewardScheme

		if state.LastEpochNumber != epoch.Number {
			cmn.Exit(cmn.Fmt("InitStateAndEpoch(), initial state error"))
		}
		state.Epoch = epoch

		rewardScheme.Save()
		epoch.Save()

	} else {
		epoch = ep.LoadOneEpoch(epochDB, state.LastEpochNumber)
		if epoch == nil {
			fmt.Printf("InitStateAndEpoch(), epoch information emitted\n")
			os.Exit(1)
		}

		state.Epoch = epoch
	}

	return state, epoch
}

//------------------------------------------------------------------------------
// Users wishing to:
//	* use an external signer for their validators
//	* supply an in-proc abci app
// should fork tendermint/tendermint and implement RunNode to
// call NewNode with their custom priv validator and/or custom
// proxy.ClientCreator interface
func RunNode(config cfg.Config, app *app.EthermintApplication) {
	// Wait until the genesis doc becomes available
	genDocFile := config.GetString("genesis_file")
	if !cmn.FileExists(genDocFile) {
		logger.Info(cmn.Fmt("Waiting for genesis file %v...", genDocFile))
		for {
			time.Sleep(time.Second)
			if !cmn.FileExists(genDocFile) {
				continue
			}
			jsonBlob, err := ioutil.ReadFile(genDocFile)
			if err != nil {
				cmn.Exit(cmn.Fmt("Couldn't read GenesisDoc file: %v", err))
			}
			genDoc, err := types.GenesisDocFromJSON(jsonBlob)
			if err != nil {
				cmn.PanicSanity(cmn.Fmt("Genesis doc parse json error: %v", err))
			}
			if genDoc.ChainID == "" {
				cmn.PanicSanity(cmn.Fmt("Genesis doc %v must include non-empty chain_id", genDocFile))
			}
			config.Set("chain_id", genDoc.ChainID)
		}
	}

	// Create & start node
	n := NewNodeDefault(config, nil)

	//protocol, address := ProtocolAndAddress(config.GetString("node_laddr"))
	//l := p2p.NewDefaultListener(protocol, address, config.GetBool("skip_upnp"))
	//n.AddListener(l)
	err := n.OnStart()
	if err != nil {
		cmn.Exit(cmn.Fmt("Failed to start node: %v", err))
	}

	//log.Notice("Started node", "nodeInfo", n.sw.NodeInfo())
	/*
		// If seedNode is provided by config, dial out.
		if config.GetString("seeds") != "" {
			seeds := strings.Split(config.GetString("seeds"), ",")
			n.DialSeeds(seeds)
		}

		// Run the RPC server.
		if config.GetString("rpc_laddr") != "" {
			_, err := n.StartRPC()
			if err != nil {
				cmn.PanicCrisis(err)
			}
		}
	*/
	// Sleep forever and then...
	cmn.TrapSignal(func() {
		n.Stop()
	})
}

//func (n *Node) NodeInfo() *p2p.NodeInfo {
//	return n.sw.NodeInfo()
//}
//
//func (n *Node) DialSeeds(seeds []string) error {
//	return n.sw.DialSeeds(n.addrBook, seeds)
//}

// Defaults to tcp
func ProtocolAndAddress(listenAddr string) (string, string) {
	protocol, address := "tcp", listenAddr
	parts := strings.SplitN(address, "://", 2)
	if len(parts) == 2 {
		protocol, address = parts[0], parts[1]
	}
	return protocol, address
}
