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

// Package p2p implements the MoacNode p2p network protocols.
package p2p

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/sha512"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MOACChain/MoacLib/common"
	"github.com/MOACChain/MoacLib/common/mclock"
	"github.com/MOACChain/MoacLib/log"
	"github.com/MOACChain/MoacVnode/p2p/discover"
	"github.com/MOACChain/MoacVnode/p2p/discv5"
	"github.com/MOACChain/MoacVnode/p2p/nat"
	"github.com/MOACChain/MoacVnode/p2p/netutil"
)

const (
	defaultDialTimeout = 15 * time.Second

	// Maximum number of concurrently handshaking inbound connections.
	maxAcceptConns = 50

	// Maximum number of concurrently dialing outbound connections.
	maxActiveDialTasks = 16

	// Maximum time allowed for reading a complete message.
	// This is effectively the amount of time a connection can be idle.
	frameReadTimeout = 30 * time.Second

	// Maximum amount of time allowed for writing a complete message.
	frameWriteTimeout = 20 * time.Second

	retrieveBootNodesLoopInterval          = 1 * time.Minute
	defaultBoostTickerLength               = 10
	defaultBoostTickerInterval             = 3 * time.Second
	defaultPersistBlacklistedNodesInterval = 1 * time.Minute
	storeSubnetBootNodeLoopInterval        = discover.KvstoreCacheUpdateInterval
)

var errServerStopped = errors.New("server stopped")

// Config holds Server options.
type Config struct {
	// This field must be set to a valid secp256k1 private key.
	PrivateKey *ecdsa.PrivateKey `toml:"-"`

	// MaxPeers is the maximum number of peers that can be
	// connected. It must be greater than zero.
	MaxPeers int

	// MaxPendingPeers is the maximum number of peers that can be pending in the
	// handshake phase, counted separately for inbound and outbound connections.
	// Zero defaults to preset values.
	MaxPendingPeers int `toml:",omitempty"`

	// NoDiscovery can be used to disable the peer discovery mechanism.
	// Disabling is useful for protocol debugging (manual topology).
	NoDiscovery bool

	// DiscoveryV5 specifies whether the the new topic-discovery based V5 discovery
	// protocol should be started or not.
	DiscoveryV5 bool `toml:",omitempty"`

	// Listener address for the V5 discovery protocol UDP traffic.
	DiscoveryV5Addr string `toml:",omitempty"`

	// Name sets the node name of this server.
	// Use common.MakeName to create a name that follows existing conventions.
	Name string `toml:"-"`

	// BootstrapNodes are used to establish connectivity
	// with the rest of the network.
	BootstrapNodes []*discover.Node

	// BootstrapNodesV5 are used to establish connectivity
	// with the rest of the network using the V5 discovery
	// protocol.
	BootstrapNodesV5 []*discv5.Node `toml:",omitempty"`

	// Static nodes are used as pre-configured connections which are always
	// maintained and re-connected on disconnects.
	StaticNodes []*discover.Node

	// Trusted nodes are used as pre-configured connections which are always
	// allowed to connect, even above the peer limit.
	TrustedNodes []*discover.Node

	// Connectivity can be restricted to certain IP networks.
	// If this option is set to a non-nil value, only hosts which match one of the
	// IP networks contained in the list are considered.
	NetRestrict *netutil.Netlist `toml:",omitempty"`

	// NodeDatabase is the path to the database containing the previously seen
	// live nodes in the network.
	NodeDatabase string `toml:",omitempty"`

	// Protocols should contain the protocols supported
	// by the server. Matching protocols are launched for
	// each peer.
	Protocols []Protocol `toml:"-"`

	// If ListenAddr is set to a non-nil address, the server
	// will listen for incoming connections.
	//
	// If the port is zero, the operating system will pick a port. The
	// ListenAddr field will be updated with the actual address when
	// the server is started.
	ListenAddr string

	// If set to a non-nil value, the given NAT port mapper
	// is used to make the listening port available to the
	// Internet.
	NAT nat.Interface `toml:",omitempty"`

	// If Dialer is set to a non-nil value, the given Dialer
	// is used to dial outbound peer connections.
	Dialer *net.Dialer `toml:"-"`

	// If NoDial is true, the server will not dial any peers.
	NoDial bool `toml:",omitempty"`

	VnodeBeneficialAddress *common.Address
	VnodeServiceCfg        *string
	ShowToPublic           bool
	Ip                     *string

	// for mainnet p2p, this field should be set to the string "mainnet"
	// otherwise, this field should be the subchain address
	// Example: "0x6274c172f15e0319e1ca2e426a0ae365b62ea64e"
	Subnet []byte

	// where we keep persisted data
	DataDir string `toml:",omitempty"`

	// Network id for this server
	NetworkId uint64

	// If node type will need to be matched exactly between remote and this node
	StrictNodeCheck bool
}

// Server manages all peer connections.
type Server struct {
	// Config fields may not be modified while the server is running.
	Config

	// Hooks for testing. These are useful because we can inhibit
	// the whole protocol stack.
	newTransport  func(net.Conn) transport
	newPeerHook   func(*Peer)
	lock          sync.Mutex // protects running
	running       bool
	ntab          discoverTable
	peerNodes     []*discover.Node
	listener      net.Listener
	ourHandshake  *protoHandshake
	lastLookup    time.Time
	DiscV5        *discv5.Network
	quit          chan struct{}
	addstatic     chan *discover.Node
	removestatic  chan *discover.Node
	addtrusted    chan *discover.Node
	removetrusted chan *discover.Node
	posthandshake chan *conn
	addpeer       chan *conn
	delpeer       chan peerDrop
	loopWG        sync.WaitGroup // loop, listenLoop

	// These are for Peers, PeerCount (and nothing else).
	peerOp     chan peerOpFunc
	peerOpDone chan struct{}

	// point to mainnet server
	MainnetServer *Server
}

type peerOpFunc func(map[discover.NodeID]*Peer)

type peerDrop struct {
	*Peer
	err       error
	requested bool // true if signaled by the peer
}

type connFlag int32

const (
	dynDialedConn connFlag = 1 << iota
	staticDialedConn
	inboundConn
	trustedConn
	validationDialedConn
)

// conn wraps a network connection with information gathered
// during the two handshakes.
type conn struct {
	fd net.Conn
	transport
	flags connFlag
	cont  chan error      // The run loop uses cont to signal errors to setupConn.
	id    discover.NodeID // valid after the encryption handshake
	caps  []Cap           // valid after the protocol handshake
	name  string          // valid after the protocol handshake
}

type transport interface {
	// The two handshakes.
	doEncHandshake(prv *ecdsa.PrivateKey, dialDest *discover.Node) (discover.NodeID, error)
	doProtoHandshake(our *protoHandshake) (*protoHandshake, []Cap, error)
	// The MsgReadWriter can only be used after the encryption
	// handshake has completed. The code uses conn.id to track this
	// by setting it to a non-nil value after the encryption handshake.
	MsgReadWriter
	// transports must provide Close because we use MsgPipe in some of
	// the tests. Closing the actual network connection doesn't do
	// anything in those tests because NsgPipe doesn't use it.
	close(err error)
}

func (c *conn) String() string {
	s := c.flags.String()
	if (c.id != discover.NodeID{}) {
		s += " " + c.id.String()
	}
	s += " " + c.fd.RemoteAddr().String()
	return s
}

func (f connFlag) String() string {
	s := ""
	if f&trustedConn != 0 {
		s += "-trusted"
	}
	if f&dynDialedConn != 0 {
		s += "-dyndial"
	}
	if f&staticDialedConn != 0 {
		s += "-staticdial"
	}
	if f&inboundConn != 0 {
		s += "-inbound"
	}
	if s != "" {
		s = s[1:]
	}
	return s
}

func (c *conn) is(f connFlag) bool {
	return c.flags&f != 0
}

func (c *conn) set(f connFlag, val bool) {
	for {
		oldFlags := connFlag(atomic.LoadInt32((*int32)(&c.flags)))
		flags := oldFlags
		if val {
			flags |= f
		} else {
			flags &= ^f
		}
		if atomic.CompareAndSwapInt32((*int32)(&c.flags), int32(oldFlags), int32(flags)) {
			return
		}
	}
}

// Get all vnodes
func (srv *Server) GetVnodes() (nodes []*discover.Node) {
	nodes = srv.ntab.GetAllNodes()
	for _, peerNode := range srv.peerNodes {
		nodes = append(nodes, peerNode)
	}
	for _, node := range srv.TrustedNodes {
		nodes = append(nodes, node)
	}
	for _, node := range srv.StaticNodes {
		nodes = append(nodes, node)
	}
	nodes = append(nodes, srv.Self())
	return nodes
}

// Peers returns all connected peers.
func (srv *Server) Peers() []*Peer {
	var ps []*Peer
	select {
	// Note: We'd love to put this function into a variable but
	// that seems to cause a weird compiler error in some
	// environments.
	case srv.peerOp <- func(peers map[discover.NodeID]*Peer) {
		for _, p := range peers {
			ps = append(ps, p)
		}
	}:
		<-srv.peerOpDone
	case <-srv.quit:
	}
	return ps
}

// PeerCount returns the number of connected peers.
func (srv *Server) PeerCount() int {
	var count int
	select {
	case srv.peerOp <- func(ps map[discover.NodeID]*Peer) { count = len(ps) }:
		<-srv.peerOpDone
	case <-srv.quit:
	}
	return count
}

// AddPeer connects to the given node and maintains the connection until the
// server is shut down. If the connection fails for any reason, the server will
// attempt to reconnect the peer.
func (srv *Server) AddPeer(node *discover.Node) {
	srv.AddPeerNode(node)
	select {
	case srv.addstatic <- node:
	case <-srv.quit:
	}
}

func (srv *Server) AddPeerNode(node *discover.Node) {
	srv.peerNodes = append(srv.peerNodes, node)
}

// RemovePeer disconnects from the given node
func (srv *Server) RemovePeer(node *discover.Node) {
	select {
	case srv.removestatic <- node:
	case <-srv.quit:
	}
}

// AddTrustedPeer adds the given node to a reserved whitelist which allows the
// node to always connect, even if the slot are full.
func (srv *Server) AddTrustedPeer(node *discover.Node) {
	select {
	case srv.addtrusted <- node:
	case <-srv.quit:
	}
}

// RemoveTrustedPeer removes the given node from the trusted peer set.
func (srv *Server) RemoveTrustedPeer(node *discover.Node) {
	select {
	case srv.removetrusted <- node:
	case <-srv.quit:
	}
}

// Self returns the local node's endpoint information.
func (srv *Server) Self() *discover.Node {
	srv.lock.Lock()
	defer srv.lock.Unlock()

	if !srv.running {
		return &discover.Node{IP: net.ParseIP("0.0.0.0")}
	}
	return srv.makeSelf(srv.listener, srv.ntab)
}

func (srv *Server) makeSelf(listener net.Listener, ntab discoverTable) *discover.Node {
	// If the server's not running, return an empty node.
	// If the node is running but discovery is off, manually assemble the node infos.
	if ntab == nil {
		// Inbound connections disabled, use zero address.
		if listener == nil {
			return &discover.Node{IP: net.ParseIP("0.0.0.0"), ID: discover.PubkeyID(&srv.PrivateKey.PublicKey)}
		}
		// Otherwise inject the listener address too
		addr := listener.Addr().(*net.TCPAddr)
		moacNode := &discover.Node{
			ID:  discover.PubkeyID(&srv.PrivateKey.PublicKey),
			IP:  addr.IP,
			TCP: uint16(addr.Port),
			UDP: uint16(addr.Port),
		}

		moacNode.SetBeneficialAddress(srv.Config.VnodeBeneficialAddress)
		moacNode.SetServiceCfg(srv.Config.VnodeServiceCfg)
		moacNode.SetShowToPublic(srv.Config.ShowToPublic)
		moacNode.SetIp(srv.Config.Ip)
		return moacNode
	}

	// Otherwise return the discovery node.
	return ntab.Self()
}

// Stop terminates the server and all active peer connections.
// It blocks until all active connections have been closed.
func (srv *Server) Stop() {
	srv.lock.Lock()
	defer srv.lock.Unlock()
	if !srv.running {
		return
	}
	srv.running = false
	if srv.listener != nil {
		// this unblocks listener Accept
		srv.listener.Close()
	}
	close(srv.quit)
	srv.loopWG.Wait()
}

// Start starts running the server.
// Servers can not be re-used after stopping.
func (srv *Server) Start() (err error) {
	srv.lock.Lock()
	defer srv.lock.Unlock()
	if srv.running {
		return errors.New("server already running")
	}
	srv.running = true
	log.Infof("P2P server for %s", string(srv.Subnet))

	// static fields
	if srv.PrivateKey == nil {
		return fmt.Errorf("Server.PrivateKey must be set to a non-nil key")
	}
	if srv.newTransport == nil {
		srv.newTransport = newRLPX
	}
	if srv.Dialer == nil {
		srv.Dialer = &net.Dialer{Timeout: defaultDialTimeout}
	}
	srv.quit = make(chan struct{})
	srv.addpeer = make(chan *conn)
	srv.delpeer = make(chan peerDrop)
	srv.posthandshake = make(chan *conn)
	srv.addstatic = make(chan *discover.Node)
	srv.removestatic = make(chan *discover.Node)
	srv.addtrusted = make(chan *discover.Node)
	srv.removetrusted = make(chan *discover.Node)
	srv.peerOp = make(chan peerOpFunc)
	srv.peerOpDone = make(chan struct{})

	// node table
	if !srv.NoDiscovery {
		discover.VnodeBeneficialAddress = srv.VnodeBeneficialAddress
		discover.VnodeServiceCfg = srv.VnodeServiceCfg
		discover.ShowToPublic = srv.ShowToPublic
		discover.Ip = srv.Ip
		ntab, err := discover.ListenUDP(
			srv.PrivateKey, srv.ListenAddr, srv.NAT,
			srv.NodeDatabase, srv.NetRestrict,
			srv.NetworkId, srv.StrictNodeCheck,
		)
		if err != nil {
			return err
		}
		if err := ntab.SetFallbackNodes(srv.BootstrapNodes); err != nil {
			return err
		}
		srv.ntab = ntab
	}

	if srv.DiscoveryV5 {
		ntab, err := discv5.ListenUDP(srv.PrivateKey, srv.DiscoveryV5Addr, srv.NAT, "", srv.NetRestrict) //srv.NodeDatabase)
		if err != nil {
			return err
		}
		if err := ntab.SetFallbackNodes(srv.BootstrapNodesV5); err != nil {
			return err
		}
		srv.DiscV5 = ntab
	}

	dynPeers := (srv.MaxPeers + 1) / 2
	if srv.NoDiscovery {
		dynPeers = 0
	}
	dialer := newDialState(srv.StaticNodes, srv.BootstrapNodes, srv.ntab, dynPeers, srv.NetRestrict)

	// handshake
	srv.ourHandshake = &protoHandshake{Version: baseProtocolVersion, Name: srv.Name, ID: discover.PubkeyID(&srv.PrivateKey.PublicKey)}
	for _, p := range srv.Protocols {
		srv.ourHandshake.Caps = append(srv.ourHandshake.Caps, p.cap())
	}
	// listen/dial
	if srv.ListenAddr != "" {
		if err := srv.startListening(); err != nil {
			return err
		}
	}
	if srv.NoDial && srv.ListenAddr == "" {
		log.Warn("P2P server will be useless, neither dialing nor listening")
	}

	// if we have blacklisted nodes persisted on disk, load it
	go srv.loadBlacklistedNodes()

	srv.loopWG.Add(1)
	go srv.run(dialer)
	srv.running = true

	log.Infof("P2P server [%s] starts listening: %v", string(srv.Subnet), srv.ListenAddr)

	// only subnet p2p server need to refresh its boot node to mainnet dht table
	if string(srv.Subnet) != "mainnet" {
		go srv.storeSubnetBootNodeLoop()
		go srv.retrieveSubnetBootNodeLoop()
	}

	return nil
}

func (srv *Server) SetNodeType(id discover.NodeID, nodeType int) {
	srv.ntab.SetNodeType(id, nodeType)
}

func (srv *Server) loadBlacklistedNodes() error {
	fileName := filepath.Join(srv.DataDir, string(srv.Subnet)+".blacklisted.nodes")
	log.Debugf("p2p server load blacklisted nodes from %s", fileName)
	fd, err := os.Open(fileName)
	defer fd.Close()
	if err == nil {
		rd := bufio.NewReader(fd)
		for {
			line, _err := rd.ReadString('\n')

			// reach end of file
			if _err == io.EOF {
				break
			}

			// read error
			if _err != nil {
				log.Debugf("read error %v for file %s", _err, fileName)
				break
			}

			// process the line, remove last "\n"
			// line in format of key,value\n
			_node := strings.Split(line[:len(line)-1], ",")
			key := _node[0]
			value, err := strconv.Atoi(_node[1])
			if err == nil {
				srv.ntab.SetNodeTypeStr(key, value)
				log.Debugf("load blacklisted node: %d, %s", value, key)
			} else {
				log.Debugf("load blacklisted node error %v", err)
			}
		}
	}

	// after we load, we can start the loop
	go srv.persistBlacklistedNodesLoop()

	return nil
}

func (srv *Server) persistBlacklistedNodesLoop() {
	ticker := time.NewTicker(defaultPersistBlacklistedNodesInterval)
	for _ = range ticker.C {
		fileName := filepath.Join(srv.DataDir, string(srv.Subnet)+".blacklisted.nodes")
		if fd, err := os.OpenFile(fileName, os.O_RDWR|os.O_CREATE, 0666); err != nil {
			log.Debugf("can not open file %s for blacklisted nodes", fileName)
		} else {
			if srv.ntab != nil {
				nodeTypeCache := srv.ntab.DumpNodeTypes()
				items := nodeTypeCache.Items()
				for key, item := range items {
					value := item.Object.(int)
					fd.WriteString(fmt.Sprintf("%s,%d\n", key, value))
				}
				fd.Close()
				log.Debugf("p2p server saved blacklisted nodes [%d] into %s", len(items), fileName)
			} else {
				log.Errorf("p2p server can not persist blacklisted nodes with nil ntab")
			}
		}
	}
}

func (srv *Server) SubnetBytesToNodeID(subnet []byte) discover.NodeID {
	var subnetID discover.NodeID
	subnetHash512 := sha512.New().Sum(subnet)
	copy(subnetID[:], subnetHash512[:])
	return subnetID
}

// lookupforsubnet returns nodes closest to the subnet id in the mainnet
func (srv *Server) lookupForSubnet(subnet []byte) ([]*discover.Node, discover.NodeID) {
	subnetID := srv.SubnetBytesToNodeID(subnet)
	// get the nodes from mainnet server
	return srv.MainnetServer.ntab.Lookup(subnetID, 0, true), subnetID
}

// retrieveSubnetBootNodeLoop() keeps refreshing bootnodes information for srv.subnet
// After retrieve bootnodes using DHT lookup, it will store the result as nursery by
// calling setfallbacknodes()
func (srv *Server) retrieveSubnetBootNodeLoop() {
	retrieveSubnetBootNode := func(_srv *Server) {
		subnetID := _srv.SubnetBytesToNodeID(_srv.Subnet)

		// check if this server's kvstore has entry for the subnet
		// it's the case where this server itself is the bootnode for the subnet p2p
		value, err := _srv.MainnetServer.ntab.GetKey(subnetID[:])
		if err == nil {
			var nodes []*discover.Node
			for _, v := range value {
				if n, _ := discover.ParseNode(v); n != nil {
					nodes = append(nodes, n)
				}
			}
			if len(nodes) > 0 {
				log.Debugf("subnet use bootnodes from local kvstore [%v]", len(nodes))
				_srv.ntab.SetFallbackNodes(nodes)
				return
			}
		}

		// if this server does not have entry, then send the findvalue msg to other nodes
		nodes, subnetID := _srv.lookupForSubnet(_srv.Subnet)
		_srv.ntab.FindBootNodes(subnetID, nodes)
		log.Debugf("subnet find bootnodes [%s]", _srv.Subnet)
	}

	// At the beginning, have shorter interval
	boostLength, boostCount := defaultBoostTickerLength, 0
	boostTicker := time.NewTicker(defaultBoostTickerInterval)
	for ; true; <-boostTicker.C {
		retrieveSubnetBootNode(srv)
		boostCount++
		if boostCount >= boostLength {
			break
		}
	}

	refreshTicker := time.NewTicker(retrieveBootNodesLoopInterval)
	for ; true; <-refreshTicker.C {
		retrieveSubnetBootNode(srv)
	}
}

// storeSubnetBootNodeLoop() keeps adding this node to other nodes' kvstore
// for the related subnet id. Otherwise, value in kvstore will expire automatically
// after some TTL
func (srv *Server) storeSubnetBootNodeLoop() {
	storeSubnetBootNode := func(_srv *Server) {
		nodes, subnetID := _srv.lookupForSubnet(_srv.Subnet)
		// refresh from subnet server (not mainnet server)
		_srv.ntab.RefreshSubnetBootNode(subnetID, nodes)
		log.Debugf("subnet refresh subnet bootnodes")
	}

	// at the beginning, have shorter interval
	boostLength, boostCount := defaultBoostTickerLength, 0
	boostTicker := time.NewTicker(defaultBoostTickerInterval)
	for ; true; <-boostTicker.C {
		storeSubnetBootNode(srv)
		boostCount++
		if boostCount >= boostLength {
			break
		}
	}

	ticker := time.NewTicker(storeSubnetBootNodeLoopInterval)
	for ; true; <-ticker.C {
		storeSubnetBootNode(srv)
	}
}

func (srv *Server) startListening() error {
	// Launch the TCP listener.
	listener, err := net.Listen("tcp", srv.ListenAddr)
	if err != nil {
		return err
	}
	laddr := listener.Addr().(*net.TCPAddr)
	srv.ListenAddr = laddr.String()
	srv.listener = listener
	srv.loopWG.Add(1)
	go srv.listenLoop()
	// Map the TCP listening port if NAT is configured.
	if !laddr.IP.IsLoopback() && srv.NAT != nil {
		srv.loopWG.Add(1)
		go func() {
			nat.Map(srv.NAT, srv.quit, "tcp", laddr.Port, laddr.Port, "moac p2p")
			srv.loopWG.Done()
		}()
	}
	return nil
}

type dialer interface {
	newTasks(running int, peers map[discover.NodeID]*Peer, now time.Time) []task
	taskDone(task, time.Time)
	addStatic(*discover.Node)
	removeStatic(*discover.Node)
}

func (srv *Server) run(dialstate dialer) {
	defer srv.loopWG.Done()
	var (
		peers        = make(map[discover.NodeID]*Peer)
		trusted      = make(map[discover.NodeID]bool, len(srv.TrustedNodes))
		taskdone     = make(chan task, maxActiveDialTasks)
		runningTasks []task
		queuedTasks  []task // tasks that can't run yet
	)
	// Put trusted nodes into a map to speed up checks.
	// Trusted peers are loaded on startup and cannot be
	// modified while the server is running.
	for _, n := range srv.TrustedNodes {
		trusted[n.ID] = true
	}

	// removes t from runningTasks
	delTask := func(t task) {
		for i := range runningTasks {
			if runningTasks[i] == t {
				runningTasks = append(runningTasks[:i], runningTasks[i+1:]...)
				break
			}
		}
	}
	// starts until max number of active tasks is satisfied
	startTasks := func(ts []task) (rest []task) {
		i := 0
		for ; len(runningTasks) < maxActiveDialTasks && i < len(ts); i++ {
			t := ts[i]
			log.Trace("New dial task", "task", t)
			go func() { t.Do(srv); taskdone <- t }()
			runningTasks = append(runningTasks, t)
		}
		return ts[i:]
	}
	scheduleTasks := func() {
		// Start from queue first.
		queuedTasks = append(queuedTasks[:0], startTasks(queuedTasks)...)
		// Query dialer for new tasks and start as many as possible now.
		if len(runningTasks) < maxActiveDialTasks {
			nt := dialstate.newTasks(len(runningTasks)+len(queuedTasks), peers, time.Now())
			queuedTasks = append(queuedTasks, startTasks(nt)...)
		}
	}

	// check ntp time
	go ntpCheck()

running:
	for {
		scheduleTasks()

		select {
		case <-srv.quit:
			// The server was stopped. Run the cleanup logic.
			break running
		case n := <-srv.addstatic:
			// This channel is used by AddPeer to add to the
			// ephemeral static peer list. Add it to the dialer,
			// it will keep the node connected.
			log.Debugf("Adding static node node=%v", n)
			dialstate.addStatic(n)
			// add it to the DHT table as well
			srv.ntab.Bond(false, n.ID, n.Addr(), uint16(n.TCP))
		case n := <-srv.removestatic:
			// This channel is used by RemovePeer to send a
			// disconnect request to a peer and begin the
			// stop keeping the node connected
			log.Debugf("Removing static node node=%v", n)
			dialstate.removeStatic(n)
			if p, ok := peers[n.ID]; ok {
				p.Disconnect(DiscRequested)
			}
		case n := <-srv.addtrusted:
			// This channel is used by AddTrustedPeer to add an enode
			// to the trusted node set.
			log.Debug("Adding trusted node", "node", n)
			trusted[n.ID] = true
			// Mark any already-connected peer as trusted
			if p, ok := peers[n.ID]; ok {
				p.rw.set(trustedConn, true)
			}
		case n := <-srv.removetrusted:
			// This channel is used by RemoveTrustedPeer to remove an enode
			// from the trusted node set.
			log.Debug("Removing trusted node", "node", n)
			if _, ok := trusted[n.ID]; ok {
				delete(trusted, n.ID)
			}
			// Unmark any already-connected peer as trusted
			if p, ok := peers[n.ID]; ok {
				p.rw.set(trustedConn, false)
			}
		case op := <-srv.peerOp:
			// This channel is used by Peers and PeerCount.
			op(peers)
			srv.peerOpDone <- struct{}{}
		case t := <-taskdone:
			// A task got done. Tell dialstate about it so it
			// can update its state and remove it from the active
			// tasks list.
			log.Trace("Dial task done", "task", t)
			dialstate.taskDone(t, time.Now())
			delTask(t)
		case c := <-srv.posthandshake:
			// A connection has passed the encryption handshake so
			// the remote identity is known (but hasn't been verified yet).
			if trusted[c.id] {
				// Ensure that the trusted flag is set before checking against MaxPeers.
				c.flags |= trustedConn
			}
			// TODO: track in-progress inbound node IDs (pre-Peer) to avoid dialing them.
			c.cont <- srv.encHandshakeChecks(peers, c)
		case c := <-srv.addpeer:
			// At this point the connection is past the protocol handshake.
			// Its capabilities are known and the remote identity is verified.
			err := srv.protoHandshakeChecks(peers, c)
			if err == nil {
				// The handshakes are done and it passed all checks.
				p := newPeer(c, srv.Protocols, srv.Subnet, srv)
				name := truncateName(c.name)
				log.Infof("Adding p2p peer (%v) id=%v name=%v addr=%v peers=%v", len(peers), c.id.String()[:16], name, c.fd.RemoteAddr(), len(peers)+1)
				peers[c.id] = p
				go srv.runPeer(p)
			}
			// The dialer logic relies on the assumption that
			// dial tasks complete after the peer has been added or
			// discarded. Unblock the task last.
			c.cont <- err
		case pd := <-srv.delpeer:
			// A peer disconnected.
			duration := common.PrettyDuration(mclock.Now() - pd.created)
			log.Infof("Removing p2p peer id=%v duration=%v peers=%v req=%v err=%v",
				pd.ID().String()[:16],
				duration,
				len(peers)-1,
				pd.requested,
				pd.err,
			)

			// if genesis block mismatch, mark the peer id as blacklisted as well
			pdErr := pd.err.Error()
			if strings.Contains(pdErr, "Genesis block mismatch") ||
				strings.Contains(pdErr, "Protocol version mismatch") ||
				strings.Contains(pdErr, "NetworkId mismatch") {
				srv.ntab.SetNodeType(pd.ID(), discover.AlienNode)
				log.Debugf(
					"Node with mismatch status [%v] = %v, will be blacklisted[%d]",
					pdErr,
					pd.ID().String()[:16],
					srv.ntab.NodeTypeSize(),
				)
			}

			delete(peers, pd.ID())
		}
	}

	log.Trace("P2P networking is spinning down")

	// Terminate discovery. If there is a running lookup it will terminate soon.
	if srv.ntab != nil {
		srv.ntab.Close()
	}
	if srv.DiscV5 != nil {
		srv.DiscV5.Close()
	}
	// Disconnect all peers.
	for _, p := range peers {
		p.Disconnect(DiscQuitting)
	}
	// Wait for peers to shut down. Pending connections and tasks are
	// not handled here and will terminate soon-ish because srv.quit
	// is closed.
	for len(peers) > 0 {
		p := <-srv.delpeer
		p.log.Trace("<-delpeer (spindown)", "remainingTasks", len(runningTasks))
		delete(peers, p.ID())
	}
}

func (srv *Server) _protoHandshakeChecks(id discover.NodeID, caps []Cap) error {
	if len(srv.Protocols) > 0 && countMatchingProtocols(srv.Protocols, caps) == 0 {
		srv.ntab.SetNodeType(id, discover.AlienNode)
		srv.ntab.DeleteWithNodeId(id)
		log.Debugf(
			"Node mismatch caps id = %v, will be blacklisted[%d]",
			id.String()[:16],
			srv.ntab.NodeTypeSize(),
		)
		return DiscUselessPeer
	}

	// known alien node
	if srv.ntab.GetNodeType(id) == discover.AlienNode {
		log.Debugf(
			"Node is known alien in proto handshake checks id = %v [%d]",
			id.String()[:16],
			srv.ntab.NodeTypeSize(),
		)
		return DiscUselessPeer
	}

	return nil
}

func (srv *Server) protoHandshakeChecks(peers map[discover.NodeID]*Peer, c *conn) error {
	// Drop connections with no matching protocols.
	if err := srv._protoHandshakeChecks(c.id, c.caps); err != nil {
		return err
	}

	// move this check from enchandshakechecks() to here
	// so that both nodes will at least send handshake msg
	// (which containes caps info) to each other before disconnect
	if !c.is(trustedConn|staticDialedConn) && len(peers) >= srv.MaxPeers {
		return DiscTooManyPeers
	}

	// Repeat the encryption handshake checks because the
	// peer set might have changed between the handshakes.
	return srv.encHandshakeChecks(peers, c)
}

func (srv *Server) encHandshakeChecks(peers map[discover.NodeID]*Peer, c *conn) error {
	switch {
	case peers[c.id] != nil:
		return DiscAlreadyConnected
	case c.id == srv.Self().ID:
		return DiscSelf
	default:
		return nil
	}
}

type tempError interface {
	Temporary() bool
}

// listenLoop runs in its own goroutine and accepts
// inbound connections.
func (srv *Server) listenLoop() {
	defer srv.loopWG.Done()
	log.Infof("RLPx listener up self=%v", srv.makeSelf(srv.listener, srv.ntab))

	// This channel acts as a semaphore limiting
	// active inbound connections that are lingering pre-handshake.
	// If all slots are taken, no further connections are accepted.
	tokens := maxAcceptConns
	if srv.MaxPendingPeers > 0 {
		tokens = srv.MaxPendingPeers
	}
	slots := make(chan struct{}, tokens)
	for i := 0; i < tokens; i++ {
		slots <- struct{}{}
	}

	for {
		// Wait for a handshake slot before accepting.
		<-slots

		var (
			fd  net.Conn
			err error
		)
		for {
			fd, err = srv.listener.Accept()
			if tempErr, ok := err.(tempError); ok && tempErr.Temporary() {
				log.Debug("Temporary read error", "err", err)
				continue
			} else if err != nil {
				log.Debug("Read error", "err", err)
				return
			}
			break
		}

		// Reject connections that do not match NetRestrict.
		if srv.NetRestrict != nil {
			if tcp, ok := fd.RemoteAddr().(*net.TCPAddr); ok && !srv.NetRestrict.Contains(tcp.IP) {
				log.Debug("Rejected conn (not whitelisted in NetRestrict)", "addr", fd.RemoteAddr())
				fd.Close()
				slots <- struct{}{}
				continue
			}
		}

		fd = newMeteredConn(fd, true)
		log.Trace("Accepted connection", "addr", fd.RemoteAddr())

		// Spawn the handler. It will give the slot back when the connection
		// has been established.
		go func() {
			srv.setupConn(fd, inboundConn, nil)
			slots <- struct{}{}
		}()
	}
}

// setupConn runs the handshakes and attempts to add the connection
// as a peer. It returns when the connection has been added as a peer
// or the handshakes have failed.
func (srv *Server) setupConn(fd net.Conn, flags connFlag, dialDest *discover.Node) {
	// Prevent leftover pending conns from entering the handshake.
	srv.lock.Lock()
	running := srv.running
	srv.lock.Unlock()
	c := &conn{fd: fd, transport: srv.newTransport(fd), flags: flags, cont: make(chan error)}
	if !running {

		c.close(errServerStopped)
		return
	}
	// Run the encryption handshake.
	var err error
	// c.id is the remote node id
	if c.id, err = c.doEncHandshake(srv.PrivateKey, dialDest); err != nil {
		c.close(err)
		return
	}
	clog := log.New("id", c.id, "moac", srv.ntab.GetNodeType(c.id), "addr", c.fd.RemoteAddr(), "conn", c.flags)
	// For dialed connections, check that the remote public key matches.
	if dialDest != nil && c.id != dialDest.ID {
		c.close(DiscUnexpectedIdentity)
		return
	}
	if err := srv.pushProgressToChan(c, srv.posthandshake); err != nil {
		c.close(err)
		return
	}
	// Run the protocol handshake
	handshakeResult, caps, err := c.doProtoHandshake(srv.ourHandshake)
	if handshakeResult != nil {
		// ignore the return error
		srv._protoHandshakeChecks(c.id, handshakeResult.Caps)
	} else {
		srv._protoHandshakeChecks(c.id, caps)
	}

	if err != nil {
		c.close(err)
		return
	}
	if handshakeResult.ID != c.id {
		c.close(DiscUnexpectedIdentity)
		return
	}
	c.caps, c.name = handshakeResult.Caps, handshakeResult.Name
	if err := srv.pushProgressToChan(c, srv.addpeer); err != nil {
		clog.Debug("[Dial Error] Rejected peer", "err", err)
		c.close(err)
		return
	}
	// If the checks completed successfully, runPeer has now been
	// launched by run.
}

func truncateName(s string) string {
	if len(s) > 20 {
		return s[:20] + "..."
	}
	return s
}

// checkpoint sends the conn to run, which performs the
// post-handshake checks for the stage (posthandshake, addpeer).
func (srv *Server) pushProgressToChan(c *conn, stage chan<- *conn) error {
	select {
	case stage <- c:
	case <-srv.quit:
		return errServerStopped
	}
	select {
	case err := <-c.cont:
		return err
	case <-srv.quit:
		return errServerStopped
	}
}

// runPeer runs in its own goroutine for each peer.
// it waits until the Peer logic returns and removes
// the peer.
func (srv *Server) runPeer(p *Peer) {
	if srv.newPeerHook != nil {
		srv.newPeerHook(p)
	}
	remoteDropRequested, err := p.run()
	// Note: run waits for existing peers to be sent on srv.delpeer
	// before returning, so this send should not select on srv.quit.
	srv.delpeer <- peerDrop{p, err, remoteDropRequested}
}

// NodeInfo represents a short summary of the information known about the host.
type NodeInfo struct {
	ID    string `json:"id"`    // Unique node identifier (also the encryption key)
	Name  string `json:"name"`  // Name of the node, including client type, version, OS, custom data
	Enode string `json:"enode"` // Enode URL for adding this peer from remote peers
	IP    string `json:"ip"`    // IP address of the node
	Ports struct {
		Discovery int `json:"discovery"` // UDP listening port for discovery protocol
		Listener  int `json:"listener"`  // TCP listening port for RLPx
	} `json:"ports"`
	ListenAddr string                 `json:"listenAddr"`
	Protocols  map[string]interface{} `json:"protocols"`
}

// NodeInfo gathers and returns a collection of metadata known about the host.
func (srv *Server) NodeInfo() *NodeInfo {
	node := srv.Self()

	// Gather and assemble the generic node infos
	info := &NodeInfo{
		Name:       srv.Name,
		Enode:      node.String(),
		ID:         node.ID.String(),
		IP:         node.IP.String(),
		ListenAddr: srv.ListenAddr,
		Protocols:  make(map[string]interface{}),
	}
	info.Ports.Discovery = int(node.UDP)
	info.Ports.Listener = int(node.TCP)

	// Gather all the running protocol infos (only once per protocol type)
	for _, proto := range srv.Protocols {
		if _, ok := info.Protocols[proto.Name]; !ok {
			nodeInfo := interface{}("unknown")
			if query := proto.NodeInfo; query != nil {
				nodeInfo = proto.NodeInfo()
			}
			info.Protocols[proto.Name] = nodeInfo
		}
	}
	return info
}

// PeersInfo returns an array of metadata objects describing connected peers.
func (srv *Server) PeersInfo() []*PeerInfo {
	// Gather all the generic and sub-protocol specific infos
	infos := make([]*PeerInfo, 0, srv.PeerCount())
	for _, peer := range srv.Peers() {
		if peer != nil {
			infos = append(infos, peer.Info())
		}
	}
	// Sort the result array alphabetically by node identifier
	for i := 0; i < len(infos); i++ {
		for j := i + 1; j < len(infos); j++ {
			if infos[i].ID > infos[j].ID {
				infos[i], infos[j] = infos[j], infos[i]
			}
		}
	}
	return infos
}
