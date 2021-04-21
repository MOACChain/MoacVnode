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

// Package discover implements the Node Discovery Protocol.
//
// The Node Discovery protocol provides a way to find RLPx nodes that
// can be connected to. It uses a Kademlia-like protocol to maintain a
// distributed database of the IDs and endpoints of all listening
// nodes.
package discover

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/MOACChain/MoacLib/common"
	"github.com/MOACChain/MoacLib/crypto"
	"github.com/MOACChain/MoacLib/log"
	gocache "github.com/patrickmn/go-cache"
)

const (
	alpha                              = 3  // Kademlia concurrency factor
	bucketSize                         = 16 // Kademlia bucket size
	hashBits                           = len(common.Hash{}) * 8
	nBuckets                           = hashBits + 1 // Number of buckets
	maxBondingPingPongs                = 16
	maxFindnodeFailures                = 5
	autoRefreshInterval                = 1 * time.Hour // seems too long, maybe change for subchain p2p network
	bucketCleanupInterval              = 30 * time.Second
	seedCount                          = 30
	seedMaxAge                         = 5 * 24 * time.Hour
	anyBytesType                uint16 = 0
	subnetBootnodeType          uint16 = 1
	subnetBootnodeKeyPrefix            = "bootnode for subnet id"
	nodeTypesCacheTTL                  = 72 * time.Hour
	nodeTypesCacheTTLMin               = 60 * time.Hour
	nodeTypesCacheTTLDropWindow        = 24 * 3600 // in seconds
	kvstoreCacheTTL                    = 5 * time.Minute
	defaultPurgeInterval               = 10 * time.Minute

	// exported
	KvstoreCacheUpdateInterval = 3 * time.Minute
)

// enum for node type, order number means more information learned about the node
const (
	UnknownNode = 0
	AlienNode   = 1
	UncleNode   = 2 // old nodes that does not support STORE and FINDVALUE udp message
	BrotherNode = 3 // new nodes that does support STORE and FINDVALUE udp message
)

const (
	BrotherOnly           = 0 // send only to brother node
	EitherUncleAndBrother = 1 // send to brother and uncle node
	AllExceptAlien        = 2 // send to brother, uncle and unknown node
)

var VnodeBeneficialAddress *common.Address
var VnodeServiceCfg *string
var ShowToPublic bool
var Ip *string

type Table struct {
	mutex      sync.Mutex        // protects buckets, their content, and nursery
	buckets    [nBuckets]*bucket // index of known nodes by distance
	nodeBucket map[NodeID]int    // mapping of node id -> bucket id
	nursery    []*Node           // bootstrap nodes
	db         *nodeDB           // database of known nodes

	refreshReq chan chan struct{}
	closeReq   chan struct{}
	closed     chan struct{}

	bondmu    sync.Mutex
	bonding   map[NodeID]*bondproc
	bondslots chan struct{} // limits total number of active bonding processes

	nodeAddedHook func(*Node) // for testing

	net  transport
	self *Node // metadata of the local node

	totalNodes uint64         // total number of nodes in the bucket
	nodeTypes  *gocache.Cache // a flag if a seen node in the p2p network a moac node

	kvstore *gocache.Cache // k/v store using gocache
}

// NodesByDistance is a list of nodes, ordered by
// distance to target.
type NodesByDistance struct {
	entries []*Node
	Target  common.Hash
}

type bondproc struct {
	err  error
	n    *Node
	done chan struct{}
}

type BootNodeCacheItem struct {
	url        string    // bootnode url in enode format
	expireTime time.Time // expire time
}

// transport is implemented by the UDP transport.
// it is an interface so we can test without opening lots of UDP
// sockets and without generating a private key.
type transport interface {
	ping(NodeID, *net.UDPAddr) error
	waitping(NodeID) error
	findnode(toid NodeID, addr *net.UDPAddr, target NodeID, strictNodeCheck bool) ([]*Node, error)
	store(key NodeID, value []byte, toNodes []*Node)
	findvalue(key NodeID, toNodes []*Node)
	getOurEndpoint() rpcEndpoint
	close()
}

// bucket contains nodes, ordered by their last activity. the entry
// that was most recently active is the first element in entries.
type bucket struct {
	entries []*Node
}

func newTable(t transport, ourID NodeID, ourAddr *net.UDPAddr, nodeDBPath string, beneficialAddress *common.Address, serviceCfg *string, showToPublic bool, ip *string) (*Table, error) {
	// If no node database was given, use an in-memory one
	db, err := newNodeDB(nodeDBPath, Version, ourID)
	if err != nil {
		return nil, err
	}
	tab := &Table{
		net:        t,
		db:         db,
		self:       NewNode(ourID, ourAddr.IP, uint16(ourAddr.Port), uint16(ourAddr.Port), beneficialAddress, serviceCfg, showToPublic, ip),
		bonding:    make(map[NodeID]*bondproc),
		bondslots:  make(chan struct{}, maxBondingPingPongs),
		refreshReq: make(chan chan struct{}),
		closeReq:   make(chan struct{}),
		closed:     make(chan struct{}),
		totalNodes: 0,
		nodeTypes:  gocache.New(nodeTypesCacheTTL, defaultPurgeInterval),
		nodeBucket: make(map[NodeID]int),
		kvstore:    gocache.New(kvstoreCacheTTL, defaultPurgeInterval),
	}
	for i := 0; i < cap(tab.bondslots); i++ {
		tab.bondslots <- struct{}{}
	}
	for i := range tab.buckets {
		tab.buckets[i] = new(bucket)
	}
	go tab.refreshLoop()
	go tab.cleanUpBuckets()
	return tab, nil
}

// Self returns the local node.
// The returned node should not be modified by the caller.
func (tab *Table) Self() *Node {
	return tab.self
}

func (tab *Table) GetAllNodes() []*Node {
	buckets := tab.buckets
	var nodes []*Node
	for _, bucket := range buckets {
		for _, node := range bucket.entries {
			nodes = append(nodes, node)
		}
	}

	return nodes
}

//NodeTypeKey() generates the key used in the nodeTypes cache
func (tab *Table) NodeTypeKey(id NodeID) string {
	return fmt.Sprintf("node type cache key: %s", id.String())
}

// GetNodeType return if a node is a moac node
func (tab *Table) GetNodeType(id NodeID) int {
	_id := tab.NodeTypeKey(id)
	if value, found := tab.nodeTypes.Get(_id); found {
		return value.(int)
	}

	// by default, treat it as non-moac node
	return UnknownNode
}

// GetNodeType return if a node is a moac node
func (tab *Table) DumpNodeTypes() *gocache.Cache {
	return tab.nodeTypes
}

// SetNodeType set if a node is a moac node
func (tab *Table) SetNodeType(id NodeID, flag int) error {
	existingNodeType := tab.GetNodeType(id)
	// if node is already set to higher type, don't downgrade it
	if existingNodeType > flag {
		return nil
	}
	_id := tab.NodeTypeKey(id)
	rand.Seed(time.Now().Unix())
	// randomly pick a expire time into the future
	// so that not all caches expire at the same time
	expireTime := nodeTypesCacheTTLMin + time.Duration(rand.Intn(nodeTypesCacheTTLDropWindow))*time.Second
	tab.nodeTypes.Set(_id, flag, expireTime)
	return nil
}

func (tab *Table) SetNodeTypeStr(id string, flag int) error {
	rand.Seed(time.Now().Unix())
	expireTime := 36*time.Hour + time.Duration(rand.Intn(3600*12))*time.Second
	tab.nodeTypes.Set(id, flag, expireTime)
	return nil
}

// SetNodeType set if a node is a moac node
func (tab *Table) NodeTypeSize() int {
	return tab.nodeTypes.ItemCount()
}

// return endpoint from udp class in the format of ip:udpport
func (tab *Table) GetOurEndpoint() string {
	endpoint := tab.net.getOurEndpoint()
	return fmt.Sprintf("%s:%d", endpoint.IP, endpoint.UDP)
}

// printkv() prints contents of a string -> string map
func printKV(_value map[string]BootNodeCacheItem) {
	i := 1
	for _, v := range _value {
		log.Debugf(
			"subnet value stored(%d/%d): %v, %v",
			i, len(_value), v.url, v.expireTime,
		)
		i++
	}
}

// GetSubnetBootnodeKey generates the key for the subnet used in tab.kvstore
func (tab *Table) GetSubnetBootnodeKey(subnet string) string {
	return fmt.Sprintf(
		"%s: %s",
		subnetBootnodeKeyPrefix,
		subnet,
	)
}

// GetKey() get the value from the kvstore using the provided key
func (tab *Table) GetKey(key []byte) (map[string]string, error) {
	bootnodeKey := tab.GetSubnetBootnodeKey(common.Bytes2Hex(key[:]))
	log.Debugf("subnet getkey [%s]", bootnodeKey)
	value, found := tab.kvstore.Get(bootnodeKey)
	if found {
		bootnodes := value.(map[string]BootNodeCacheItem)
		notExpiredBootnodes := make(map[string]BootNodeCacheItem)
		returnBootnodes := make(map[string]string)
		now := time.Now()
		for k, bootnode := range bootnodes {
			if bootnode.expireTime.After(now) {
				notExpiredBootnodes[k] = bootnode
				returnBootnodes[k] = bootnode.url
			}
		}
		tab.kvstore.Set(bootnodeKey, notExpiredBootnodes, gocache.DefaultExpiration)
		return returnBootnodes, nil
	} else {
		return map[string]string{}, fmt.Errorf("Can not find key in table's kvstore: [%s]", bootnodeKey)
	}
}

// SetKey() set the key/value to the kvstore
// fromID is used to double check node ID and print out debug information
func (tab *Table) SetKey(key []byte, value []byte, fromID NodeID) bool {
	// key is subnet id, value is enode url
	// store a nodeID -> enode url mapping
	log.Debugf(
		"subnet store bootnode %s (%v)",
		fmt.Sprintf("%s", fromID)[:16],
		tab.db.lastPong(fromID),
	)
	nodeURL := string(value)
	var n *Node
	if n, _ = ParseNode(nodeURL); n == nil {
		log.Debugf("subnet can not parse node url in setkey: %s", nodeURL)
		return false
	}

	// double check if nodeID and fromID matches
	nodeID := common.Bytes2Hex(n.ID[:])
	if nodeID != common.Bytes2Hex(fromID[:]) {
		log.Debugf("subnet fromID and enode url parsed ndoe id does not match: %v, %v", nodeID, common.Bytes2Hex(fromID[:]))
		return false
	}

	cachekey := tab.GetSubnetBootnodeKey(common.Bytes2Hex(key[:]))
	existValue, found := tab.kvstore.Get(cachekey)
	log.Debugf("subnet setkey [%s]", cachekey)
	// if key already exists
	if found {
		// not enough slots: find the oldest node in the map,
		// and ping it, if it does not response, replace it with the
		// value from new node, otherwise do nothing
		bootnodes := existValue.(map[string]BootNodeCacheItem)
		notExpiredBootnodes := make(map[string]BootNodeCacheItem)
		now := time.Now()
		for k, bootnode := range bootnodes {
			if bootnode.expireTime.After(now) {
				notExpiredBootnodes[k] = bootnode
			}
		}
		if len(notExpiredBootnodes) >= 16 {
			var oldest *Node
			oldestLastPong := time.Now()
			for id, _ := range notExpiredBootnodes {
				nodeid, err := HexID(id)
				if err == nil {
					nodeLastpong := tab.db.lastPong(nodeid)
					if oldestLastPong.After(nodeLastpong) {
						oldestLastPong = nodeLastpong
						oldest = tab.db.node(nodeid)
					}
				}
			}
			if oldest == nil {
				return false
			}
			err := tab.ping(oldest.ID, oldest.addr())
			// if the node does not response, replace it with the new one
			// otherwise, do not replace
			if err != nil {
				nodeid := common.Bytes2Hex(oldest.ID[:])
				delete(notExpiredBootnodes, nodeid)
				fromid := common.Bytes2Hex(fromID[:])
				bootnodeItem := BootNodeCacheItem{
					url:        nodeURL,
					expireTime: time.Now().Add(kvstoreCacheTTL),
				}
				notExpiredBootnodes[fromid] = bootnodeItem
				tab.kvstore.Set(cachekey, notExpiredBootnodes, gocache.DefaultExpiration)
				printKV(notExpiredBootnodes)
			}
		} else {
			// still have slots left, just update
			fromid := common.Bytes2Hex(fromID[:])
			bootnodeItem := BootNodeCacheItem{
				url:        nodeURL,
				expireTime: time.Now().Add(kvstoreCacheTTL),
			}
			notExpiredBootnodes[fromid] = bootnodeItem
			tab.kvstore.Set(cachekey, notExpiredBootnodes, gocache.DefaultExpiration)
			printKV(notExpiredBootnodes)
		}
	} else {
		// if not found, just store the value
		bootnodes := make(map[string]BootNodeCacheItem)
		bootnodeItem := BootNodeCacheItem{
			url:        nodeURL,
			expireTime: time.Now().Add(kvstoreCacheTTL),
		}
		bootnodes[nodeID] = bootnodeItem
		tab.kvstore.Set(cachekey, bootnodes, gocache.DefaultExpiration)
		printKV(bootnodes)
	}
	return true
}

// ReadRandomNodes fills the given slice with random nodes from the
// table. It will not write the same node more than once. The nodes in
// the slice are copies and can be modified by the caller.
func (tab *Table) ReadRandomNodes(buf []*Node) (n int) {
	tab.mutex.Lock()
	defer tab.mutex.Unlock()
	// TODO: tree-based buckets would help here
	// Find all non-empty buckets and get a fresh slice of their entries.
	var buckets [][]*Node
	for _, b := range tab.buckets {
		if len(b.entries) > 0 {
			buckets = append(buckets, b.entries[:])
		}
	}
	if len(buckets) == 0 {
		return 0
	}
	// Shuffle the buckets.
	for i := uint32(len(buckets)) - 1; i > 0; i-- {
		j := randUint(i)
		buckets[i], buckets[j] = buckets[j], buckets[i]
	}
	// Move head of each bucket into buf, removing buckets that become empty.
	var i, j int
	for ; i < len(buf); i, j = i+1, (j+1)%len(buckets) {
		b := buckets[j]
		buf[i] = &(*b[0])
		buckets[j] = b[1:]
		if len(b) == 1 {
			buckets = append(buckets[:j], buckets[j+1:]...)
		}
		if len(buckets) == 0 {
			break
		}
	}
	return i + 1
}

func randUint(max uint32) uint32 {
	if max == 0 {
		return 0
	}
	var b [4]byte
	rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:]) % max
}

// Close terminates the network listener and flushes the node database.
func (tab *Table) Close() {
	select {
	case <-tab.closed:
		// already closed.
	case tab.closeReq <- struct{}{}:
		<-tab.closed // wait for refreshLoop to end.
	}
}

// SetFallbackNodes sets the initial points of contact. These nodes
// are used to connect to the network if the table is empty and there
// are no known nodes in the database.
func (tab *Table) SetFallbackNodes(nodes []*Node) error {
	for _, n := range nodes {
		if err := n.validateComplete(); err != nil {
			return fmt.Errorf("bad bootstrap/fallback node %q (%v)", n, err)
		}
	}
	tab.mutex.Lock()
	log.Debugf("tab nursery before(%d -> %d): %v, %v", len(tab.nursery), len(nodes), tab.nursery, nodes)
	tab.nursery = make([]*Node, 0, len(nodes))
	for _, n := range nodes {
		cpy := *n
		// Recompute cpy.sha because the node might not have been
		// created by NewNode or ParseNode.
		cpy.sha = crypto.Keccak256Hash(n.ID[:])
		tab.nursery = append(tab.nursery, &cpy)
	}
	log.Debugf("tab nursery after(%d): %v", len(tab.nursery), tab.nursery)
	tab.mutex.Unlock()
	tab.refresh()
	return nil
}

// Resolve searches for a specific node with the given ID.
// It returns nil if the node could not be found.
func (tab *Table) Resolve(targetID NodeID) *Node {
	// If the node is present in the local table, no
	// network interaction is required.
	hash := crypto.Keccak256Hash(targetID[:])
	tab.mutex.Lock()

	cl := tab.closest(
		hash, 1, AllExceptAlien,
	)
	tab.mutex.Unlock()
	if len(cl.entries) > 0 && cl.entries[0].ID == targetID {
		return cl.entries[0]
	}
	// Otherwise, do a network lookup.
	result := tab.Lookup(targetID, 0, false)
	for _, n := range result {
		if n.ID == targetID {
			return n
		}
	}
	return nil
}

// Lookup performs a network search for nodes close
// to the given target. It approaches the target by querying
// nodes that are closer to it on each iteration.
// The given target does not need to be an actual node
// identifier.
func (tab *Table) Lookup(
	targetID NodeID, lookupID int, strictNodeCheck bool,
) []*Node {
	return tab.lookup(targetID, true, lookupID, strictNodeCheck)
}

func printNodesByDist(nodeByDist *NodesByDistance, where string, lookupID int, targetID NodeID) {
	log.Debugf("[%d]subnet nodes by distance ================ [%s][%s]", lookupID, where, targetID.String()[:32])
	for i, node := range nodeByDist.entries {
		log.Debugf("subnet nodes by distance [%d/%d] %v", i+1, len(nodeByDist.entries), node)
	}
}

func (tab *Table) lookup(
	targetID NodeID, refreshIfEmpty bool,
	lookupID int, strictNodeCheck bool,
) []*Node {
	var (
		target          = crypto.Keccak256Hash(targetID[:])
		asked           = make(map[NodeID]bool)
		seen            = make(map[NodeID]bool)
		bondedNodesChan = make(chan []*Node, alpha)
		pendingQueries  = 0
		nodesByDist     *NodesByDistance
	)
	// don't query further if we hit ourself.
	// unlikely to happen often in practice.
	asked[tab.self.ID] = true

	for {
		tab.mutex.Lock()
		// generate initial nodesByDist set
		// at the begining, nodesbydist will contain only brother nodes
		// but it will include unknown nodes as well later
		matchType := EitherUncleAndBrother
		if strictNodeCheck {
			matchType = BrotherOnly
		}
		nodesByDist = tab.closest(
			target, bucketSize, matchType,
		)
		for _, node := range nodesByDist.entries {
			seen[node.ID] = true
		}
		tab.mutex.Unlock()
		if len(nodesByDist.entries) > 0 || !refreshIfEmpty {
			break
		}
		// The nodesByDist set is empty, all nodes were dropped, refresh.
		// We actually wait for the refresh to complete here. The very
		// first query will hit this case and run the bootstrapping
		// logic.
		<-tab.refresh()
		refreshIfEmpty = false
	}
	log.Debugf("all found nodes closest result: %v, strict=%t", nodesByDist.entries, strictNodeCheck)

	findNodeCount := 0
	for {
		// ask the alpha closest nodes that we haven't asked yet
		for i := 0; i < len(nodesByDist.entries) && pendingQueries < alpha; i++ {
			node := nodesByDist.entries[i]
			if !asked[node.ID] {
				asked[node.ID] = true
				pendingQueries++
				go func() {
					// Find potential neighbors to bond with
					// nodes in reply will be filter by getNodeType()
					foundNodes, err := tab.net.findnode(node.ID, node.addr(), targetID, strictNodeCheck)
					totalFoundNodes := len(foundNodes)
					log.Debugf("all found nodes of %d from %v:", totalFoundNodes, node)
					for i, node := range foundNodes {
						log.Debugf("all found nodes [%d/%d]: %v with target: %s", i+1, totalFoundNodes, node, targetID.String()[:16])
					}
					findNodeCount++
					// handle error
					if err != nil {
						// Bump the failure counter to detect and evacuate non-bonded entries
						fails := tab.db.findFails(node.ID) + 1
						tab.db.updateFindFails(node.ID, fails)
						log.Trace("Bumping findnode failure counter", "id", node.ID, "failcount", fails, "err:", err)

						if fails >= maxFindnodeFailures {
							log.Trace("Too many findnode failures, dropping", "id", node.ID, "failcount", fails)
							tab.delete(node)
						}
					}
					bondedNodesChan <- tab.bondall(foundNodes)
				}()
			}
		}
		if pendingQueries == 0 {
			// we have asked all closest nodes, stop the search
			break
		}
		// wait for the next bondedNodesChan
		// keep updating nodesByDist with fresh reply from other nodes
		// and cap the size of it by bucketSize, eventually it will have
		// up to bucketSize number of nodes, all of which are closest to
		// the target in the whole p2p network.
		for _, node := range <-bondedNodesChan {
			if node != nil && !seen[node.ID] {
				seen[node.ID] = true
				// if we know it is alien node, don't send findnode msg
				// this can NOT be node == BrotherNode, because we need to
				// first add the unknown node into the bucket for us to
				// try to handshake in TCP to figure out if the node is
				// brother or not.
				if tab.GetNodeType(node.ID) != AlienNode {
					nodesByDist.Push(node, bucketSize)
				}
			}
		}
		pendingQueries--
		printNodesByDist(nodesByDist, "network search", lookupID, targetID)
	}
	return nodesByDist.entries
}

func (tab *Table) refresh() <-chan struct{} {
	done := make(chan struct{})
	select {
	case tab.refreshReq <- done:
	case <-tab.closed:
		close(done)
	}
	return done
}

// cleanUpBuckets runs periodically and looks for eth node that's in the buckets and remove it from buckets.
func (tab *Table) cleanUpBuckets() {
	ticker := time.NewTicker(bucketCleanupInterval)
	rounds := 0
	for ; true; <-ticker.C {
		var nodesToDelete []*Node
		alienNode := 0
		unknownNode := 0
		brotherNode := 0
		uncleNode := 0

		for _, bucket := range tab.buckets {
			for _, node := range bucket.entries {
				switch nodeType := tab.GetNodeType(node.ID); nodeType {
				case AlienNode:
					nodesToDelete = append(nodesToDelete, node)
					alienNode++
				case UnknownNode:
					// clear unknown nodes every 4 rounds
					if rounds%4 == 0 {
						nodesToDelete = append(nodesToDelete, node)
					}
					unknownNode++
				case BrotherNode:
					brotherNode++
				case UncleNode:
					uncleNode++
				}
			}
		}

		nodes := tab.GetAllNodes()
		totalNodes := len(nodes)
		log.Debugf(
			"all nodes in bucket: %d, alien: %d, unknown: %d, brother: %d, uncle: %d",
			totalNodes,
			alienNode,
			unknownNode,
			brotherNode,
			uncleNode,
		)

		for _, node := range nodesToDelete {
			log.Debugf("cleanup buckets, node remove: %v", node)
			tab.delete(node)
		}

		nodes = tab.GetAllNodes()
		totalNodes = len(nodes)
		for i, node := range nodes {
			log.Debugf(
				"all nodes in bucket: [%d/%d, %d] %v",
				i+1,
				totalNodes,
				tab.GetNodeType(node.ID),
				node,
			)
		}

		rounds++
	}
}

func (tab *Table) RefreshSubnetBootNode(subnetID NodeID, nodesToRefresh []*Node) {
	// subnetid in nodeID format so that we use the same lookup function
	for i, node := range nodesToRefresh {
		log.Debugf("send subnet bootnode to nodes[%d/%d]: %v", i+1, len(nodesToRefresh), node)
	}
	// just send empty byte array as value
	tab.net.store(subnetID, []byte{}, nodesToRefresh)
}

func (tab *Table) FindBootNodes(subnetID NodeID, toNodes []*Node) {
	tab.net.findvalue(subnetID, toNodes)
}

// refreshLoop schedules doRefresh runs and coordinates shutdown.
func (tab *Table) refreshLoop() {
	var (
		timer   = time.NewTicker(autoRefreshInterval)
		waiting []chan struct{} // accumulates waiting callers while doRefresh runs
		done    chan struct{}   // where doRefresh reports completion
	)
loop:
	for {
		select {
		case <-timer.C:
			if done == nil {
				done = make(chan struct{})
				go tab.doRefresh(done)
			}
		case req := <-tab.refreshReq:
			waiting = append(waiting, req)
			if done == nil {
				done = make(chan struct{})
				go tab.doRefresh(done)
			}
		case <-done:
			for _, ch := range waiting {
				close(ch)
			}
			waiting = nil
			done = nil
		case <-tab.closeReq:
			break loop
		}
	}

	if tab.net != nil {
		tab.net.close()
	}
	if done != nil {
		<-done
	}
	for _, ch := range waiting {
		close(ch)
	}
	tab.db.close()
	close(tab.closed)
}

// doRefresh performs a lookup for a random target to keep buckets
// full. seed nodes are inserted if the table is empty (initial
// bootstrap or discarded faulty peers).
func (tab *Table) doRefresh(done chan struct{}) {
	defer close(done)

	// The Kademlia paper specifies that the bucket refresh should
	// perform a lookup in the least recently used bucket. We cannot
	// adhere to this because the findnode target is a 512bit value
	// (not hash-sized) and it is not easily possible to generate a
	// sha3 preimage that falls into a chosen bucket.
	// We perform a lookup with a random target instead.
	var target NodeID
	rand.Read(target[:])
	result := tab.lookup(target, false, 0, false)
	if len(result) > 0 {
		return
	}

	// The table is empty. Load nodes from the database and insert
	// them. This should yield a few previously seen nodes that are
	// (hopefully) still alive.
	seeds := tab.db.querySeeds(seedCount, seedMaxAge)
	seeds = tab.bondall(append(seeds, tab.nursery...))

	if len(seeds) == 0 {
		log.Debug("No discv4 seed nodes found")
	}
	for _, n := range seeds {
		age := log.Lazy{Fn: func() time.Duration { return time.Since(tab.db.lastPong(n.ID)) }}
		log.Trace("Found seed node in database", "id", n.ID, "addr", n.addr(), "age", age)
	}
	tab.mutex.Lock()
	tab.stuff(seeds)
	tab.mutex.Unlock()

	// Finally, do a self lookup to fill up the buckets.
	tab.lookup(tab.self.ID, false, 0, false)
}

// closest returns the n nodes in the table that are closest to the
// given id. The caller must hold tab.mutex.
func (tab *Table) closest(
	target common.Hash, nresults int, matchType int,
) *NodesByDistance {
	// This is a very wasteful way to find the closest nodes but
	// obviously correct. I believe that tree-based buckets would make
	// this easier to implement efficiently.
	close := &NodesByDistance{Target: target}
	for _, b := range tab.buckets {
		for _, n := range b.entries {
			// filter out all unknown or alien nodes
			// this affects how do we
			// 1) send findnode msg to which nodes
			// 2) reply to received findnode msg
			nodeType := tab.GetNodeType(n.ID)
			switch matchType {
			case EitherUncleAndBrother:
				if nodeType == UncleNode || nodeType == BrotherNode {
					close.Push(n, nresults)
				}
			case BrotherOnly:
				if nodeType == BrotherNode {
					close.Push(n, nresults)
				}
			case AllExceptAlien:
				if nodeType != AlienNode {
					close.Push(n, nresults)
				}
			}
		}
	}
	return close
}

func (tab *Table) len() (n int) {
	for _, b := range tab.buckets {
		n += len(b.entries)
	}
	return n
}

// bondall bonds with all given nodes concurrently and returns
// those nodes for which bonding has probably succeeded.
func (tab *Table) bondall(nodes []*Node) (bondedNodes []*Node) {
	rc := make(chan *Node, len(nodes))
	for i := range nodes {
		go func(n *Node) {
			nn, _ := tab.bond(false, n.ID, n.addr(), uint16(n.TCP))
			rc <- nn
		}(nodes[i])
	}
	// sync all nodes bondedNodes before we return
	for range nodes {
		if n := <-rc; n != nil {
			bondedNodes = append(bondedNodes, n)
		}
	}
	return bondedNodes
}

// Public version of the bond() below
func (tab *Table) Bond(pinged bool, id NodeID, addr *net.UDPAddr, tcpPort uint16) (*Node, error) {
	return tab.bond(pinged, id, addr, tcpPort)
}

// bond ensures the local node has a bond with the given remote node.
// It also attempts to insert the node into the table if bonding succeeds.
// The caller must not hold tab.mutex.
//
// A bond is must be established before sending findnode requests.
// Both sides must have completed a ping/pong exchange for a bond to
// exist. The total number of active bonding processes is limited in
// order to restrain network use.
//
// bond is meant to operate idempotently in that bonding with a remote
// node which still remembers a previously established bond will work.
// The remote node will simply not send a ping back, causing waitping
// to time out.
//
// If pinged is true, the remote node has just pinged us and one half
// of the process can be skipped.
func (tab *Table) bond(pinged bool, id NodeID, addr *net.UDPAddr, tcpPort uint16) (*Node, error) {
	if id == tab.self.ID {
		return nil, errors.New("is self")
	}

	// Retrieve a previously known node and any recent findnode failures
	node, fails := tab.db.node(id), 0
	if node != nil {
		fails = tab.db.findFails(id)
	}
	// If the node is unknown (non-bonded) or failed (remotely unknown), bond from scratch
	var result error
	age := time.Since(tab.db.lastPong(id))
	if node == nil || fails > 0 || age > nodeDBNodeExpiration {
		log.Trace(
			"Starting bonding ping/pong", "id",
			id, "known", node != nil, "failcount", fails,
			"age", age,
		)

		tab.bondmu.Lock()
		w := tab.bonding[id]
		if w != nil {
			// Wait for an existing bonding process to complete.
			tab.bondmu.Unlock()
			<-w.done
		} else {
			// Register a new bonding process.
			w = &bondproc{done: make(chan struct{})}
			tab.bonding[id] = w
			tab.bondmu.Unlock()
			// Do the ping/pong. The result goes into w.
			tab.pingpong(w, pinged, id, addr, tcpPort)
			// Unregister the process after it's done.
			tab.bondmu.Lock()
			delete(tab.bonding, id)
			tab.bondmu.Unlock()
		}
		// Retrieve the bonding results
		result = w.err
		if result == nil {
			node = w.n
		}
	}
	if node != nil {
		// Add the node to the table even if the bonding ping/pong
		// fails. It will be relaced quickly if it continues to be
		// unresponsive.
		tab.add(node)
		tab.db.updateFindFails(id, 0)
	}
	return node, result
}

func (tab *Table) pingpong(w *bondproc, pinged bool, id NodeID, addr *net.UDPAddr, tcpPort uint16) {
	// Request a bonding slot to limit network usage
	<-tab.bondslots
	defer func() { tab.bondslots <- struct{}{} }()

	// Ping the remote side and wait for a pong.
	if w.err = tab.ping(id, addr); w.err != nil {
		close(w.done)
		return
	}
	if !pinged {
		// Give the remote node a chance to ping us before we start
		// sending findnode requests. If they still remember us,
		// waitping will simply time out.
		tab.net.waitping(id)
	}
	// Bonding succeeded, update the node database.
	w.n = NewNode(id, addr.IP, uint16(addr.Port), tcpPort, nil, nil, false, nil)
	tab.db.updateNode(w.n)
	close(w.done)
}

// ping a remote endpoint and wait for a reply, also updating the node
// database accordingly.
func (tab *Table) ping(id NodeID, addr *net.UDPAddr) error {
	tab.db.updateLastPing(id, time.Now())
	if err := tab.net.ping(id, addr); err != nil {
		return err
	}
	tab.db.updateLastPong(id, time.Now())

	// Start the background expiration goroutine after the first
	// successful communication. Subsequent calls have no effect if it
	// is already running. We do this here instead of somewhere else
	// so that the search for seed nodes also considers older nodes
	// that would otherwise be removed by the expiration.
	tab.db.ensureExpirer()
	return nil
}

// add attempts to add the given node its corresponding bucket. If the
// bucket has space available, adding the node succeeds immediately.
// Otherwise, the node is added if the least recently active node in
// the bucket does not respond to a ping packet.
//
// The caller must not hold tab.mutex.
func (tab *Table) add(new *Node) {
	if tab.GetNodeType(new.ID) == AlienNode {
		log.Debugf("node bucket add abort, not brother node %s", new.ID.String()[:16])
		return
	}

	bIndex := logdist(tab.self.sha, new.sha)
	b := tab.buckets[bIndex]
	tab.mutex.Lock()
	defer tab.mutex.Unlock()
	// if node already exist in the bucket, bump it to the front
	if b.bump(new) {
		return
	}

	// otherwise, find the oldest node and see if we can replace
	var oldest *Node
	// if bucket is full
	if len(b.entries) == bucketSize {
		oldest = b.entries[bucketSize-1]
		if oldest.contested {
			// The node is already being replaced, don't attempt
			// to replace it.
			return
		}
		oldest.contested = true
		// Let go of the mutex so other goroutines can access
		// the table while we ping the least recently active node.
		tab.mutex.Unlock()
		err := tab.ping(oldest.ID, oldest.addr())
		tab.mutex.Lock()
		oldest.contested = false
		if err == nil {
			// The node responded, don't replace it.
			return
		}
	}
	replaced := b.replace(new, oldest)
	if replaced {
		tab.nodeBucket[new.ID] = bIndex
		if tab.nodeAddedHook != nil {
			tab.nodeAddedHook(new)
		}

	}
}

// stuff adds nodes the table to the end of their corresponding bucket
// if the bucket is not full. The caller must hold tab.mutex.
func (tab *Table) stuff(nodes []*Node) {
outer:
	for _, node := range nodes {
		if node.ID == tab.self.ID {
			continue // don't add self
		}
		bIndex := logdist(tab.self.sha, node.sha)
		bucket := tab.buckets[bIndex]
		for i := range bucket.entries {
			if bucket.entries[i].ID == node.ID {
				continue outer // already in bucket
			}
		}
		if len(bucket.entries) < bucketSize {
			bucket.entries = append(bucket.entries, node)
			tab.totalNodes++
			tab.nodeBucket[node.ID] = bIndex
			if tab.nodeAddedHook != nil {
				tab.nodeAddedHook(node)
			}
		}
	}
}

// delete removes an entry from the node table (used to evacuate
// failed/non-bonded discovery peers).
func (tab *Table) delete(node *Node) {
	tab.mutex.Lock()
	defer tab.mutex.Unlock()
	bIndex := logdist(tab.self.sha, node.sha)
	tab._deleteWithNodeIdAndIndex(node.ID, bIndex)
}

func (tab *Table) DeleteWithNodeId(id NodeID) {
	tab.mutex.Lock()
	defer tab.mutex.Unlock()
	if bIndex, found := tab.nodeBucket[id]; found {
		tab._deleteWithNodeIdAndIndex(id, bIndex)
	}
}

// Do not use this function directly since it does not hold tab.mutex
// use the above two delete functions
func (tab *Table) _deleteWithNodeIdAndIndex(id NodeID, index int) {
	bucket := tab.buckets[index]
	for i := range bucket.entries {
		if bucket.entries[i].ID == id {
			bucket.entries = append(bucket.entries[:i], bucket.entries[i+1:]...)
			tab.totalNodes--
			delete(tab.nodeBucket, id)
			return
		}
	}
}

func (b *bucket) replace(n *Node, last *Node) bool {
	// Don't add if b already contains n.
	for i := range b.entries {
		if b.entries[i].ID == n.ID {
			return false
		}
	}
	// Replace last if it is still the last entry or just add n if b
	// isn't full. If is no longer the last entry, it has either been
	// replaced with someone else or became active.
	if len(b.entries) == bucketSize && (last == nil || b.entries[bucketSize-1].ID != last.ID) {
		return false
	}
	if len(b.entries) < bucketSize {
		b.entries = append(b.entries, nil)
	}
	copy(b.entries[1:], b.entries)
	b.entries[0] = n
	return true
}

func (b *bucket) bump(n *Node) bool {
	for i := range b.entries {
		if b.entries[i].ID == n.ID {
			// move it to the front
			copy(b.entries[1:], b.entries[:i])
			b.entries[0] = n
			return true
		}
	}
	return false
}

// push adds the given node to the list, keeping the total size below maxElems.
func (h *NodesByDistance) GetEntries() []*Node {
	return h.entries
}

// push adds the given node to the list, keeping the total size below maxElems.
func (h *NodesByDistance) Push(n *Node, maxElems int) {
	ix := sort.Search(len(h.entries), func(i int) bool {
		return distcmp(h.Target, h.entries[i].sha, n.sha) > 0
	})
	if len(h.entries) < maxElems {
		h.entries = append(h.entries, n)
	}
	if ix == len(h.entries) {
		// farther away than all nodes we already have.
		// if there was room for it, the node is now the last element.
	} else {
		// slide existing entries down to make room
		// this will overwrite the entry we just appended.
		copy(h.entries[ix+1:], h.entries[ix:])
		h.entries[ix] = n
	}
}
