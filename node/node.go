// Copyright 2015 The go-ethereum Authors
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

package node

import (
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MOACChain/MoacLib/common"
	"github.com/MOACChain/MoacLib/log"
	"github.com/MOACChain/MoacLib/mcdb"
	"github.com/MOACChain/MoacLib/params"
	"github.com/MOACChain/MoacVnode/accounts"
	"github.com/MOACChain/MoacVnode/event"
	"github.com/MOACChain/MoacVnode/internal/debug"
	"github.com/MOACChain/MoacVnode/p2p"
	"github.com/MOACChain/MoacVnode/p2p/discover"
	"github.com/MOACChain/MoacVnode/rpc"
	"github.com/prometheus/prometheus/util/flock"
)

const (
	MAINNET_SERVER_CHECK_INTERVAL = 3 * time.Second
	MAINNET_SERVER_CHECK_TIMEOUT  = 10
)

// Node is a container on which services can be registered.
type Node struct {
	eventmux *event.TypeMux // Event multiplexer used between the services of a stack
	config   *Config
	accman   *accounts.Manager

	ephemeralKeystore string         // if non-empty, the key directory that will be removed by Stop
	instanceDirLock   flock.Releaser // prevents concurrent use of instance directory

	serverConfig  p2p.Config
	server        *p2p.Server            // Currently running P2P networking layer for mainnet
	subnetServers map[string]*p2p.Server // Currently running P2P networking layer for subnets

	serviceFuncs []ServiceConstructor     // Service constructors (in dependency order)
	services     map[reflect.Type]Service // Currently running services

	rpcAPIs       []rpc.API   // List of APIs currently provided by the node
	inprocHandler *rpc.Server // In-process RPC request handler to process the API requests

	ipcEndpoint string       // IPC endpoint to listen at (empty = IPC disabled)
	ipcListener net.Listener // IPC RPC listener socket to serve API requests
	ipcHandler  *rpc.Server  // IPC RPC request handler to process the API requests

	httpEndpoint  string       // HTTP endpoint (interface + port) to listen at (empty = HTTP disabled)
	httpWhitelist []string     // HTTP RPC modules to allow through this endpoint
	httpListener  net.Listener // HTTP RPC listener socket to server API requests
	httpHandler   *rpc.Server  // HTTP RPC request handler to process the API requests

	wsEndpoint string       // Websocket endpoint (interface + port) to listen at (empty = websocket disabled)
	wsListener net.Listener // Websocket RPC listener socket to server API requests
	wsHandler  *rpc.Server  // Websocket RPC request handler to process the API requests

	requestNonce uint64

	stop chan struct{} // Channel to wait for termination notifications
	lock sync.RWMutex
}

var nd *Node

// export these configs so that they can be set/update by other modules
var VnodeBeneficialAddress *common.Address
var VnodeServiceCfg *string
var ShowToPublic bool
var Ip *string
var ForceSubnetP2P []string

// New creates a new P2P node, ready for protocol registration.
// Can use this to replace SCS?
func New(conf *Config) (*Node, error) {
	// Copy config and resolve the datadir so future changes to the current
	// working directory don't affect the node.
	confCopy := *conf
	conf = &confCopy
	if conf.DataDir != "" {
		absdatadir, err := filepath.Abs(conf.DataDir)
		if err != nil {
			return nil, err
		}
		conf.DataDir = absdatadir
	}

	// Ensure that the instance name doesn't cause weird conflicts with
	// other files in the data directory.
	if strings.ContainsAny(conf.Name, `/\`) {
		return nil, errors.New(`Config.Name must not contain '/' or '\'`)
	}
	if conf.Name == datadirDefaultKeyStore {
		return nil, errors.New(`Config.Name cannot be "` + datadirDefaultKeyStore + `"`)
	}
	if strings.HasSuffix(conf.Name, ".ipc") {
		return nil, errors.New(`Config.Name cannot end in ".ipc"`)
	}
	// Ensure that the AccountManager method works before the node has started.
	// We rely on this in cmd/moac.
	am, ephemeralKeystore, err := makeAccountManager(conf)
	if err != nil {
		return nil, err
	}
	// Note: any interaction with Config that would create/touch files
	// in the data directory or instance directory is delayed until Start.
	nd = &Node{
		accman:            am,
		ephemeralKeystore: ephemeralKeystore,
		config:            conf,
		serviceFuncs:      []ServiceConstructor{},
		ipcEndpoint:       conf.IPCEndpoint(),
		httpEndpoint:      conf.HTTPEndpoint(),
		wsEndpoint:        conf.WSEndpoint(),
		eventmux:          new(event.TypeMux),
		requestNonce:      0,
		subnetServers:     make(map[string]*p2p.Server),
	}
	return nd, nil
}

func GetInstance() *Node {
	return nd
}

// Register injects a new service into the node's stack. The service created by
// the passed constructor must be unique in its type with regard to sibling ones.
func (n *Node) Register(constructor ServiceConstructor) error {
	n.lock.Lock()
	defer n.lock.Unlock()

	if n.server != nil {
		return ErrNodeRunning
	}
	n.serviceFuncs = append(n.serviceFuncs, constructor)
	return nil
}

// randomly select a listen port between min and max port number
func (n *Node) GetSubnetServerListenPort() string {
	var dup bool
	r := ""
	for {
		dup = false
		rand.Seed(time.Now().UnixNano())
		r = fmt.Sprintf(
			"%s:%d",
			params.VnodeIP,
			rand.Intn(params.SubnetListenPortMax-params.SubnetListenPortMin)+params.SubnetListenPortMin,
		)
		for _, server := range n.subnetServers {
			if r == server.Config.ListenAddr {
				dup = true
			}
		}

		if !dup {
			return r
		}
	}
}

func (n *Node) HasSubnetServer(subnetid string) bool {
	ret := n.subnetServers[subnetid] != nil
	log.Debugf("subnet has server for %s: %t", subnetid, ret)
	return ret
}

func (n *Node) waitForMainnetServer(subnetid string) bool {
	// if the mainnet p2p server is not created, we can't create subnet p2p server
	// check every x seconds if mainnet server is ready, timeout in y seconds
	ticker := time.NewTicker(MAINNET_SERVER_CHECK_INTERVAL)
	counter := 0
	for _ = range ticker.C {
		if n.server == nil || n.services == nil {
			log.Debugf("subnet mainnet p2p server is missing for %s", subnetid)
			counter += 1
		} else {
			log.Debugf("subnet mainnet p2p server is found for %s", subnetid)
			break
		}

		if counter == MAINNET_SERVER_CHECK_TIMEOUT {
			log.Error("mainnet p2p server is missing subnet creation failed.")
			return true
		}
	}

	return false
}

func (n *Node) StartSubnetP2PServer(subnetid string) {
	// if the subnet p2p server is created, don't create it again
	if n.subnetServers[subnetid] != nil {
		log.Warn("subnet p2p server has been created already for %s", subnetid)
		return
	}

	// if timeout, just return
	if timeout := n.waitForMainnetServer(subnetid); timeout {
		return
	}

	log.Infof("Start subnet p2p server for subnet %s", subnetid)
	// should sync, could be called in concurrent go thread
	subnetServerConfig := n.config.P2P
	// subnet should use strict node check
	subnetServerConfig.StrictNodeCheck = true
	// setup bootnode for test only, in prod the value should be get from dht table lookup
	subnetServerConfig.BootstrapNodes = []*discover.Node{}
	subnetServerConfig.MaxPeers = n.config.P2P.MaxPeers/params.SubnetP2PConnectionFraction + 1
	if subnetServerConfig.MaxPeers < params.SubnetP2PConnectionMin {
		subnetServerConfig.MaxPeers = params.SubnetP2PConnectionMin
	}
	subnetServerConfig.ListenAddr = n.GetSubnetServerListenPort() // e.g. ":40333"
	if n.serverConfig.NodeDatabase == "" {
		n.serverConfig.NodeDatabase = n.config.NodeDB(subnetid)
	}

	// set private key if it's not set explicitly by --nodekey
	if subnetServerConfig.PrivateKey == nil {
		subnetServerConfig.PrivateKey = n.config.NodeKey()
	}

	subnetP2PServer := &p2p.Server{
		Config:        subnetServerConfig,
		MainnetServer: n.server,
	}
	log.Debugf("subnet id %s, subnet p2p server listen addr %s", subnetid, subnetP2PServer.ListenAddr)
	subnetP2PServer.Subnet = []byte(subnetid)

	for _, service := range n.services {
		subnetP2PServer.Protocols = append(subnetP2PServer.Protocols, service.Protocols()...)
	}
	if err := subnetP2PServer.Start(); err != nil {
		log.Errorf("subnet p2p server start failed with error %v for %s", err, subnetid)
		return
	}

	log.Infof("Started peer-to-peer node for %s", subnetid)
	n.subnetServers[subnetid] = subnetP2PServer
}

// Start create a live P2P node and starts running it.
func (n *Node) Start() error {
	n.lock.Lock()
	defer n.lock.Unlock()

	// Short circuit if the node's already running
	if n.server != nil {
		return ErrNodeRunning
	}
	if err := n.openDataDir(); err != nil {
		return err
	}

	// Initialize the p2p server. This creates the node key and
	// discovery databases.
	n.serverConfig = n.config.P2P
	n.serverConfig.PrivateKey = n.config.NodeKey()
	n.serverConfig.Name = n.config.NodeName()
	if n.serverConfig.StaticNodes == nil {
		n.serverConfig.StaticNodes = n.config.StaticNodes()
	}
	if n.serverConfig.TrustedNodes == nil {
		n.serverConfig.TrustedNodes = n.config.TrustedNodes()
	}
	if n.serverConfig.NodeDatabase == "" {
		n.serverConfig.NodeDatabase = n.config.NodeDB("mainnet")
	}
	n.serverConfig.DataDir = n.config.DataDir
	n.serverConfig.VnodeServiceCfg = VnodeServiceCfg
	n.serverConfig.VnodeBeneficialAddress = VnodeBeneficialAddress
	n.serverConfig.ShowToPublic = ShowToPublic
	n.serverConfig.Ip = Ip
	// do not use strict node check in mainnet
	n.serverConfig.StrictNodeCheck = false
	p2pServer := &p2p.Server{Config: n.serverConfig}
	p2pServer.Subnet = []byte("mainnet")
	log.Info("Starting peer-to-peer node", "instance", n.serverConfig.Name)

	// Copy and specialize the P2P configuration
	services := make(map[reflect.Type]Service)
	for _, constructor := range n.serviceFuncs {
		// Create a new context for the particular service
		ctx := &ServiceContext{
			config:         n.config,
			services:       make(map[reflect.Type]Service),
			EventMux:       n.eventmux,
			AccountManager: n.accman,
		}
		for kind, s := range services { // copy needed for threaded access
			ctx.services[kind] = s
		}
		// Construct and save the service
		service, err := constructor(ctx)
		if err != nil {
			return err
		}
		kind := reflect.TypeOf(service)
		if _, exists := services[kind]; exists {
			return &DuplicateServiceError{Kind: kind}
		}
		services[kind] = service
	}

	// Gather the protocols and start the freshly assembled P2P server
	i := 1
	for _type, service := range services {
		log.Debugf("subnet service (%d/%d) %v, %v, %v", i, len(services), _type, service, service.Protocols())
		i++
		p2pServer.Protocols = append(p2pServer.Protocols, service.Protocols()...)
	}
	if err := p2pServer.Start(); err != nil {
		return convertFileLockError(err)
	}

	// Start each of the services
	started := []reflect.Type{}
	for kind, service := range services {
		// Start the next service, stopping all previous upon failure
		log.Infof("Start service %s, %v", kind, service)
		// for moacservice, p2pserver is used to initialize the net rpc api
		if err := service.Start(p2pServer); err != nil {
			for _, kind := range started {
				services[kind].Stop()
			}
			p2pServer.Stop()

			return err
		}
		// Mark the service started for potential cleanup
		started = append(started, kind)
	}
	// Lastly start the configured RPC interfaces
	if err := n.startRPC(services); err != nil {
		for _, service := range services {
			service.Stop()
		}
		p2pServer.Stop()
		return err
	}
	// Finish initializing the startup
	n.services = services
	n.server = p2pServer
	n.stop = make(chan struct{})

	return nil
}

func (n *Node) openDataDir() error {
	if n.config.DataDir == "" {
		return nil // ephemeral
	}

	instdir := filepath.Join(n.config.DataDir, n.config.name())
	if err := os.MkdirAll(instdir, 0700); err != nil {
		return err
	}
	// Lock the instance directory to prevent concurrent use by another instance as well as
	// accidental use of the instance directory as a database.
	release, _, err := flock.New(filepath.Join(instdir, "LOCK"))
	if err != nil {
		return convertFileLockError(err)
	}
	n.instanceDirLock = release
	return nil
}

// startRPC is a helper method to start all the various RPC endpoint during node
// startup. It's not meant to be called at any time afterwards as it makes certain
// assumptions about the state of the node.
func (n *Node) startRPC(services map[reflect.Type]Service) error {
	// Gather all the possible APIs to surface
	apis := n.apis()
	for _, service := range services {
		apis = append(apis, service.APIs()...)
	}
	// Start the various API endpoints, terminating all in case of errors
	if err := n.startInProc(apis); err != nil {
		return err
	}
	if err := n.startIPC(apis); err != nil {
		n.stopInProc()
		return err
	}

	if err := n.startHTTP(n.httpEndpoint, apis, n.config.HTTPModules, n.config.HTTPCors); err != nil {
		n.stopIPC()
		n.stopInProc()
		return err
	}
	if err := n.startWS(n.wsEndpoint, apis, n.config.WSModules, n.config.WSOrigins); err != nil {
		n.stopHTTP()
		n.stopIPC()
		n.stopInProc()
		return err
	}
	// All API endpoints started successfully
	n.rpcAPIs = apis
	return nil
}

// startInProc initializes an in-process RPC endpoint.
func (n *Node) startInProc(apis []rpc.API) error {
	// Register all the APIs exposed by the services
	handler := rpc.NewServer()
	for _, api := range apis {
		if err := handler.RegisterName(api.Namespace, api.Service); err != nil {
			return err
		}
		log.Debug(fmt.Sprintf("InProc registered %T under '%s'", api.Service, api.Namespace))
	}
	n.inprocHandler = handler
	return nil
}

// stopInProc terminates the in-process RPC endpoint.
func (n *Node) stopInProc() {
	if n.inprocHandler != nil {
		n.inprocHandler.Stop()
		n.inprocHandler = nil
	}
}

// startIPC initializes and starts the IPC RPC endpoint.
func (n *Node) startIPC(apis []rpc.API) error {
	log.Info("[node/node.go->Node.startIPC]")
	// Short circuit if the IPC endpoint isn't being exposed
	if n.ipcEndpoint == "" {
		return nil
	}
	// Register all the APIs exposed by the services
	handler := rpc.NewServer()
	for _, api := range apis {
		if err := handler.RegisterName(api.Namespace, api.Service); err != nil {
			return err
		}
	}
	// All APIs registered, start the IPC listener
	var (
		listener net.Listener
		err      error
	)
	if listener, err = rpc.CreateIPCListener(n.ipcEndpoint); err != nil {
		return err
	}
	go func() {
		log.Info(fmt.Sprintf("IPC endpoint opened: %s", n.ipcEndpoint))

		for {
			conn, err := listener.Accept()
			if err != nil {
				// Terminate if the listener was closed
				n.lock.RLock()
				closed := n.ipcListener == nil
				n.lock.RUnlock()
				if closed {
					return
				}
				// Not closed, just some error; report and continue
				log.Error(fmt.Sprintf("IPC accept failed: %v", err))
				continue
			}
			log.Info("[node/node.go->Node.startIPC call ServeCodec]")
			go handler.ServeCodec(rpc.NewJSONCodec(conn), rpc.OptionMethodInvocation|rpc.OptionSubscriptions)
		}
	}()
	// All listeners booted successfully
	n.ipcListener = listener
	n.ipcHandler = handler

	return nil
}

// stopIPC terminates the IPC RPC endpoint.
func (n *Node) stopIPC() {
	if n.ipcListener != nil {
		n.ipcListener.Close()
		n.ipcListener = nil

		log.Info(fmt.Sprintf("IPC endpoint closed: %s", n.ipcEndpoint))
	}
	if n.ipcHandler != nil {
		n.ipcHandler.Stop()
		n.ipcHandler = nil
	}
}

// startHTTP initializes and starts the HTTP RPC endpoint.
func (n *Node) startHTTP(endpoint string, apis []rpc.API, modules []string, cors []string) error {
	// Short circuit if the HTTP endpoint isn't being exposed
	if endpoint == "" {
		return nil
	}
	// Generate the whitelist based on the allowed modules
	whitelist := make(map[string]bool)
	for _, module := range modules {
		whitelist[module] = true
	}
	// Register all the APIs exposed by the services
	handler := rpc.NewServer()
	for _, api := range apis {
		if whitelist[api.Namespace] || (len(whitelist) == 0 && api.Public) {
			if err := handler.RegisterName(api.Namespace, api.Service); err != nil {
				return err
			}
		}
	}
	// All APIs registered, start the HTTP listener
	var (
		listener net.Listener
		err      error
	)
	if listener, err = net.Listen("tcp", endpoint); err != nil {
		return err
	}
	go rpc.NewHTTPServer(cors, handler).Serve(listener)
	log.Info(fmt.Sprintf("HTTP endpoint opened: http://%s", endpoint))

	// All listeners booted successfully
	n.httpEndpoint = endpoint
	n.httpListener = listener
	n.httpHandler = handler

	return nil
}

// stopHTTP terminates the HTTP RPC endpoint.
func (n *Node) stopHTTP() {
	if n.httpListener != nil {
		n.httpListener.Close()
		n.httpListener = nil

		log.Info(fmt.Sprintf("HTTP endpoint closed: http://%s", n.httpEndpoint))
	}
	if n.httpHandler != nil {
		n.httpHandler.Stop()
		n.httpHandler = nil
	}
}

// startWS initializes and starts the websocket RPC endpoint.
func (n *Node) startWS(endpoint string, apis []rpc.API, modules []string, wsOrigins []string) error {
	// Short circuit if the WS endpoint isn't being exposed
	if endpoint == "" {
		return nil
	}
	// Generate the whitelist based on the allowed modules
	whitelist := make(map[string]bool)
	for _, module := range modules {
		whitelist[module] = true
	}
	// Register all the APIs exposed by the services
	handler := rpc.NewServer()
	for _, api := range apis {
		if whitelist[api.Namespace] || (len(whitelist) == 0 && api.Public) {
			if err := handler.RegisterName(api.Namespace, api.Service); err != nil {
				return err
			}
			log.Debug(fmt.Sprintf("WebSocket registered %T under '%s'", api.Service, api.Namespace))
		}
	}
	// All APIs registered, start the HTTP listener
	var (
		listener net.Listener
		err      error
	)
	if listener, err = net.Listen("tcp", endpoint); err != nil {
		return err
	}
	go rpc.NewWSServer(wsOrigins, handler).Serve(listener)
	log.Info(fmt.Sprintf("WebSocket endpoint opened: ws://%s", endpoint))

	// All listeners booted successfully
	n.wsEndpoint = endpoint
	n.wsListener = listener
	n.wsHandler = handler

	return nil
}

// stopWS terminates the websocket RPC endpoint.
func (n *Node) stopWS() {
	if n.wsListener != nil {
		n.wsListener.Close()
		n.wsListener = nil

		log.Info(fmt.Sprintf("WebSocket endpoint closed: ws://%s", n.wsEndpoint))
	}
	if n.wsHandler != nil {
		n.wsHandler.Stop()
		n.wsHandler = nil
	}
}

// Stop terminates a running node along with all it's services. In the node was
// not started, an error is returned.
func (n *Node) Stop() error {
	n.lock.Lock()
	defer n.lock.Unlock()

	// Short circuit if the node's not running
	if n.server == nil {
		return ErrNodeStopped
	}

	// Terminate the API, services and the p2p server.
	n.stopWS()
	n.stopHTTP()
	n.stopIPC()
	n.rpcAPIs = nil
	failure := &StopError{
		Services: make(map[reflect.Type]error),
	}
	for kind, service := range n.services {
		if err := service.Stop(); err != nil {
			failure.Services[kind] = err
		}
	}
	n.server.Stop()
	for subnetid, subnetServer := range n.subnetServers {
		subnetServer.Stop()
		delete(n.subnetServers, subnetid)
	}
	n.services = nil
	n.server = nil

	// Release instance directory lock.
	if n.instanceDirLock != nil {
		if err := n.instanceDirLock.Release(); err != nil {
			log.Error("Can't release datadir lock", "err", err)
		}
		n.instanceDirLock = nil
	}

	// unblock n.Wait
	close(n.stop)

	// Remove the keystore if it was created ephemerally.
	var keystoreErr error
	if n.ephemeralKeystore != "" {
		keystoreErr = os.RemoveAll(n.ephemeralKeystore)
	}

	if len(failure.Services) > 0 {
		return failure
	}
	if keystoreErr != nil {
		return keystoreErr
	}
	return nil
}

// Stop subnet server by subnet id
func (n *Node) StopSubnetServer(subnetid string) {
	if subnetServer, found := n.subnetServers[subnetid]; found {
		subnetServer.Stop()
		delete(n.subnetServers, subnetid)
	}
}

// Wait blocks the thread until the node is stopped. If the node is not running
// at the time of invocation, the method immediately returns.
func (n *Node) Wait() {
	n.lock.RLock()
	if n.server == nil {
		n.lock.RUnlock()
		return
	}
	stop := n.stop
	n.lock.RUnlock()

	<-stop
}

// Attach creates an RPC client attached to an in-process API handler.
func (n *Node) Attach() (*rpc.Client, error) {
	n.lock.RLock()
	defer n.lock.RUnlock()

	if n.server == nil {
		return nil, ErrNodeStopped
	}
	return rpc.DialInProc(n.inprocHandler), nil
}

// Server retrieves the currently running P2P network layer. This method is meant
// only to inspect fields of the currently running server, life cycle management
// should be left to this Node entity.
func (n *Node) Server() *p2p.Server {
	n.lock.RLock()
	defer n.lock.RUnlock()

	return n.server
}

// Service retrieves a currently running service registered of a specific type.
func (n *Node) Service(service interface{}) error {
	n.lock.RLock()
	defer n.lock.RUnlock()

	// Short circuit if the node's not running
	if n.server == nil {
		return ErrNodeStopped
	}
	// Otherwise try to find the service to return
	element := reflect.ValueOf(service).Elem()
	if running, ok := n.services[element.Type()]; ok {
		element.Set(reflect.ValueOf(running))
		return nil
	}
	return ErrServiceUnknown
}

// DataDir retrieves the current datadir used by the protocol stack.
// Deprecated: No files should be stored in this directory, use InstanceDir instead.
func (n *Node) DataDir() string {
	return n.config.DataDir
}

// InstanceDir retrieves the instance directory used by the protocol stack.
func (n *Node) InstanceDir() string {
	return n.config.instanceDir()
}

// AccountManager retrieves the account manager used by the protocol stack.
func (n *Node) AccountManager() *accounts.Manager {
	return n.accman
}

// IPCEndpoint retrieves the current IPC endpoint used by the protocol stack.
func (n *Node) IPCEndpoint() string {
	return n.ipcEndpoint
}

// HTTPEndpoint retrieves the current HTTP endpoint used by the protocol stack.
func (n *Node) HTTPEndpoint() string {
	return n.httpEndpoint
}

// WSEndpoint retrieves the current WS endpoint used by the protocol stack.
func (n *Node) WSEndpoint() string {
	return n.wsEndpoint
}

// EventMux retrieves the event multiplexer used by all the network services in
// the current protocol stack.
func (n *Node) EventMux() *event.TypeMux {
	return n.eventmux
}

// OpenDatabase opens an existing database with the given name (or creates one if no
// previous can be found) from within the node's instance directory. If the node is
// ephemeral, a memory database is returned.
func (n *Node) OpenDatabase(name string, cache, handles int) (mcdb.Database, error) {
	if n.config.DataDir == "" {
		return mcdb.NewMemDatabase()
	}
	return mcdb.NewLDBDatabase(n.config.resolvePath(name), cache, handles)
}

// ResolvePath returns the absolute path of a resource in the instance directory.
func (n *Node) ResolvePath(x string) string {
	return n.config.resolvePath(x)
}

// apis returns the collection of RPC descriptors this node offers.
func (n *Node) apis() []rpc.API {
	return []rpc.API{
		{
			Namespace: "admin",
			Version:   "1.0",
			Service:   NewPrivateAdminAPI(n),
		}, {
			Namespace: "admin",
			Version:   "1.0",
			Service:   NewPublicAdminAPI(n),
			Public:    true,
		}, {
			Namespace: "debug",
			Version:   "1.0",
			Service:   debug.Handler,
		}, {
			Namespace: "debug",
			Version:   "1.0",
			Service:   NewPublicDebugAPI(n),
			Public:    true,
		}, {
			Namespace: "chain3",
			Version:   "1.0",
			Service:   NewPublicChain3API(n),
			Public:    true,
		},
	}
}

func (n *Node) UpdateP2P() {
	n.serverConfig.VnodeServiceCfg = VnodeServiceCfg
	n.serverConfig.VnodeBeneficialAddress = VnodeBeneficialAddress
	n.serverConfig.ShowToPublic = ShowToPublic
	n.serverConfig.Ip = Ip
}

func (n *Node) GetRequestId(newFlag bool) ([]byte, error) {
	p2pServer := n.Server()
	if p2pServer == nil {
		return []byte{}, fmt.Errorf("No p2p server")
	}
	nodeInfo := p2pServer.NodeInfo()
	id := nodeInfo.ID
	if !newFlag {
		return []byte(id), nil
	} else {
		n.requestNonce++
		requestId := id + "-" + strconv.FormatUint(n.requestNonce, 10)
		return []byte(requestId), nil
	}
}
