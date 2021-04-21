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

package discover

import (
	"bytes"
	"container/list"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/MOACChain/MoacLib/common"
	"github.com/MOACChain/MoacLib/crypto"
	"github.com/MOACChain/MoacLib/log"
	"github.com/MOACChain/MoacLib/params"
	"github.com/MOACChain/MoacLib/rlp"
	"github.com/MOACChain/MoacVnode/p2p/nat"
	"github.com/MOACChain/MoacVnode/p2p/netutil"
)

const Version = 4
const BootNodeLimits = 5

// Errors
var (
	errPacketTooSmall   = errors.New("too small")
	errBadHash          = errors.New("bad hash")
	errExpired          = errors.New("expired")
	errUnsolicitedReply = errors.New("unsolicited reply")
	errUnknownNode      = errors.New("unknown node")
	errTimeout          = errors.New("RPC timeout")
	errClockWarp        = errors.New("reply deadline too far in the future")
	errClosed           = errors.New("socket closed")
)

// Timeouts
const (
	respTimeout = 500 * time.Millisecond
	expiration  = 20 * time.Second

	ntpFailureThreshold = 32               // Continuous timeouts after which to check NTP
	ntpWarningCooldown  = 10 * time.Minute // Minimum amount of time to pass before repeating NTP warning
	driftThreshold      = 10 * time.Second // Allowed clock drift before warning user
)

// RPC packet types
const (
	PINGPACKET = iota + 1 // zero is 'reserved'
	PONGPACKET
	FINDNODEPACKET
	NEIGHBORSPACKET
	STOREPACKET
	STOREREPLYPACKET
	FINDVALUEPACKET
	FINDVALUEREPLYPACKET
)

// RPC request structures
type (
	ping struct {
		Version    uint
		From, To   rpcEndpoint
		Expiration uint64
		// Ignore additional fields (for forward compatibility).
		Rest []rlp.RawValue `rlp:"tail"`
	}

	// pong is the reply to ping.
	pong struct {
		// This field should mirror the UDP envelope address
		// of the ping packet, which provides a way to discover the
		// the external address (after NAT).
		To rpcEndpoint

		ReplyTok   []byte // This contains the hash of the ping packet.
		Expiration uint64 // Absolute timestamp at which the packet becomes invalid.
		// Ignore additional fields (for forward compatibility).
		Rest []rlp.RawValue `rlp:"tail"`
	}

	// findnode is a query for nodes close to the given target.
	findnode struct {
		Target     NodeID // doesn't need to be an actual public key
		Expiration uint64
		// Ignore additional fields (for forward compatibility).
		Rest []rlp.RawValue `rlp:"tail"`
	}

	// store is a query for nodes to store a key/value to nodes in the dht network
	store struct {
		Key        NodeID      // doesn't need to be an actual public key, just 64 bytes array like hash
		From       rpcEndpoint // origin source ip/port
		Expiration uint64      // Absolute timestamp at which the packet becomes invalid.
		Value      []byte      // value to be stored
	}

	// storeReply is a query for nodes to store a key/value to nodes in the dht network
	storeReply struct {
		Expiration uint64 // Absolute timestamp at which the packet becomes invalid.
		result     bool   // whether store kv is successful or not
	}

	findvalue struct {
		Key        NodeID // doesn't need to be an actual public key
		Expiration uint64 // Absolute timestamp at which the packet becomes invalid.
	}

	findvalueReply struct {
		Key        []byte // key to the value
		Value      []byte // value to be retrived.
		Expiration uint64 // Absolute timestamp at which the packet becomes invalid.
	}

	// reply to findnode
	neighbors struct {
		Nodes      []rpcNode
		Expiration uint64
		// Ignore additional fields (for forward compatibility).
		Rest []rlp.RawValue `rlp:"tail"`
	}

	rpcNode struct {
		IP                net.IP // len 4 for IPv4 or 16 for IPv6
		UDP               uint16 // for discovery protocol
		TCP               uint16 // for RLPx protocol
		ID                NodeID
		beneficialAddress *common.Address
		serviceCfg        *string
		showToPublic      bool
		ip                *string
	}

	rpcEndpoint struct {
		IP  net.IP // len 4 for IPv4 or 16 for IPv6
		UDP uint16 // for discovery protocol
		TCP uint16 // for RLPx protocol
	}
)

func makeEndpoint(addr *net.UDPAddr, tcpPort uint16) rpcEndpoint {
	ip := addr.IP.To4()
	if ip == nil {
		ip = addr.IP.To16()
	}
	return rpcEndpoint{IP: ip, UDP: uint16(addr.Port), TCP: tcpPort}
}

func (u *udp) nodeFromRPC(sender *net.UDPAddr, rn rpcNode) (*Node, error) {
	if rn.UDP <= 1024 {
		return nil, errors.New("low port")
	}
	if err := netutil.CheckRelayIP(sender.IP, rn.IP); err != nil {
		return nil, err
	}
	if u.netrestrict != nil && !u.netrestrict.Contains(rn.IP) {
		return nil, errors.New("not contained in netrestrict whitelists")
	}
	n := NewNode(rn.ID, rn.IP, rn.UDP, rn.TCP, rn.beneficialAddress, rn.serviceCfg, rn.showToPublic, rn.ip)
	err := n.validateComplete()
	return n, err
}

func nodeToRPC(n *Node) rpcNode {
	return rpcNode{
		ID:  n.ID,
		IP:  n.IP,
		UDP: n.UDP,
		TCP: n.TCP,
	}
}

type packet interface {
	handle(u *udp, from *net.UDPAddr, fromID NodeID, mac []byte) error
	name() string
}

type conn interface {
	ReadFromUDP(b []byte) (n int, addr *net.UDPAddr, err error)
	WriteToUDP(b []byte, addr *net.UDPAddr) (n int, err error)
	Close() error
	LocalAddr() net.Addr
}

// udp implements the RPC protocol.
type udp struct {
	conn            conn
	netrestrict     *netutil.Netlist
	priv            *ecdsa.PrivateKey
	ourEndpoint     rpcEndpoint
	pendings        chan *pending
	gotreply        chan reply
	closing         chan struct{}
	nat             nat.Interface
	networkid       uint64
	strictNodeCheck bool
	*Table
}

// pending represents a pending reply.
//
// some implementations of the protocol wish to send more than one
// reply packet to findnode. in general, any neighbors packet cannot
// be matched up with a specific findnode packet.
//
// our implementation handles this by storing a callback function for
// each pending reply. incoming packets from a node are dispatched
// to all the callback functions for that node.
type pending struct {
	// these fields must match in the reply.
	from  NodeID
	ptype byte

	// time when the request must complete
	deadline time.Time

	// callback is called when a matching reply arrives. if it returns
	// true, the callback is removed from the pending reply queue.
	// if it returns false, the reply is considered incomplete and
	// the callback will be invoked again for the next matching reply.
	callback func(resp interface{}) (done bool)

	// errc receives nil when the callback indicates completion or an
	// error if no further reply is received within the timeout.
	errc chan<- error

	// when this pending is created
	createAt time.Time
}

type reply struct {
	from  NodeID
	ptype byte
	data  interface{}
	// loop indicates whether there was
	// a matching request by sending on this channel.
	matched chan<- bool
}

// ListenUDP returns a new table that listens for UDP packets on laddr.
func ListenUDP(
	priv *ecdsa.PrivateKey,
	laddr string,
	natm nat.Interface,
	nodeDBPath string,
	netrestrict *netutil.Netlist,
	networkid uint64,
	strictNodeCheck bool,
) (*Table, error) {
	addr, err := net.ResolveUDPAddr("udp", laddr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, err
	}

	tab, _, err := newUDP(
		priv, conn, natm, nodeDBPath,
		netrestrict, networkid, strictNodeCheck,
	)
	if err != nil {
		return nil, err
	}
	log.Infof("UDP listener up self=%v", tab.self)

	return tab, nil
}

func newUDP(
	priv *ecdsa.PrivateKey,
	c conn,
	natm nat.Interface,
	nodeDBPath string,
	netrestrict *netutil.Netlist,
	networkid uint64,
	strictNodeCheck bool,
) (*Table, *udp, error) {
	udp := &udp{
		conn:            c,
		priv:            priv,
		netrestrict:     netrestrict,
		closing:         make(chan struct{}),
		gotreply:        make(chan reply),
		pendings:        make(chan *pending),
		networkid:       networkid,
		strictNodeCheck: strictNodeCheck,
	}
	realaddr := c.LocalAddr().(*net.UDPAddr)
	if natm != nil {
		if !realaddr.IP.IsLoopback() {
			go nat.Map(natm, udp.closing, "udp", realaddr.Port, realaddr.Port, "moac discovery")
		}
		// TODO: react to external IP changes over time.
		if ext, err := natm.ExternalIP(); err == nil {
			realaddr = &net.UDPAddr{IP: ext, Port: realaddr.Port}
		}
	}
	// TODO: separate TCP port
	udp.ourEndpoint = makeEndpoint(realaddr, uint16(realaddr.Port))
	tab, err := newTable(udp, PubkeyID(&priv.PublicKey), realaddr, nodeDBPath, VnodeBeneficialAddress, VnodeServiceCfg, ShowToPublic, Ip)
	if err != nil {
		return nil, nil, err
	}
	udp.Table = tab
	log.Debugf("udp listen on: %v, %v", realaddr, uint16(realaddr.Port))

	go udp.loop()
	go udp.readLoop()
	return udp.Table, udp, nil
}

func (u *udp) close() {
	close(u.closing)
	u.conn.Close()
	// TODO: wait for the loops to end.
}

// The following three functions: ping, waitping, findnode
// are the interface of the transport defined in table.go

func (u *udp) getOurEndpoint() rpcEndpoint {
	return u.ourEndpoint
}

// ping sends a ping message to the given node and waits for a reply.
func (u *udp) ping(toid NodeID, toaddr *net.UDPAddr) error {
	// TODO: maybe check for ReplyTo field in callback to measure RTT
	errc := u.addPending(toid, PONGPACKET, func(interface{}) bool { return true })
	msg, _ := rlp.EncodeToBytes(fmt.Sprintf("%d\t", u.networkid))
	Rest := []rlp.RawValue{msg}
	u.send(toid, toaddr, PINGPACKET, &ping{
		Version:    Version,
		From:       u.ourEndpoint,
		To:         makeEndpoint(toaddr, 0), // TODO: maybe use known TCP port from DB
		Expiration: uint64(time.Now().Add(expiration).Unix()),
		Rest:       Rest,
	})
	log.Debugf(">> PING our point: %v, remote point: %v", u.ourEndpoint, toaddr)
	return <-errc
}

func (u *udp) waitping(from NodeID) error {
	return <-u.addPending(from, PINGPACKET, func(interface{}) bool { return true })
}

// findnode sends a findnode request to the given node and waits until
// the node has sent up to k neighbors.
func (u *udp) findnode(toid NodeID, toaddr *net.UDPAddr, target NodeID, strictNodeCheck bool) ([]*Node, error) {
	nodes := make([]*Node, 0, bucketSize)
	nreceived := 0
	errc := u.addPending(
		toid,
		NEIGHBORSPACKET,
		func(r interface{}) bool {
			// this is the callback function which is called
			// upon receiving neighbors reply
			reply := r.(*neighbors)
			discarded := 0
			for _, rn := range reply.Nodes {
				nreceived++
				n, err := u.nodeFromRPC(toaddr, rn)
				if err != nil {
					log.Trace("Invalid neighbor node received", "ip", rn.IP, "addr", toaddr, "err", err)
					continue
				}
				// if we know the neighbor is a alien client, filter it out
				if u.Table.GetNodeType(n.ID) == AlienNode {
					discarded++
				} else {
					// in strict mode, only brother node get appended
					if strictNodeCheck {
						if u.Table.GetNodeType(n.ID) == BrotherNode {
							nodes = append(nodes, n)
						}
					} else {
						nodes = append(nodes, n)
					}
				}
			}
			log.Debugf("in findnode discarded %d/%d", discarded, nreceived)
			return nreceived >= bucketSize
		},
	)

	// set content in rest
	strictNodeCheckInt := 0
	if strictNodeCheck {
		strictNodeCheckInt = 1
	}
	msg, _ := rlp.EncodeToBytes(fmt.Sprintf("%d", strictNodeCheckInt))
	Rest := []rlp.RawValue{msg}

	// send msg
	u.send(toid, toaddr, FINDNODEPACKET, &findnode{
		Target:     target,
		Expiration: uint64(time.Now().Add(expiration).Unix()),
		Rest:       Rest,
	})
	err := <-errc

	return nodes, err
}

// we treat every key as node id since they are in the same id space,
// this is now specifically coded to handle encode urls, but maybe we
// should make this function more general.
func (u *udp) findvalue(key NodeID, toNodes []*Node) {
	for _, node := range toNodes {
		go func(_key NodeID, _node *Node) {
			errc := u.addPending(
				_node.ID,
				FINDVALUEREPLYPACKET,
				func(r interface{}) bool {
					reply := r.(*findvalueReply)
					results := strings.Split(string(reply.Value), ",")
					log.Debugf(
						"subnet receive findvalue reply from: %v, %v", nil, results,
					)
					var nodes []*Node
					for _, nodeURL := range results {
						if n, _ := ParseNode(nodeURL); n != nil {
							nodes = append(nodes, n)
						}
					}
					u.Table.SetFallbackNodes(nodes)
					return true
				},
			)
			u.send(_node.ID, _node.addr(), FINDVALUEPACKET, &findvalue{
				Key:        _key,
				Expiration: uint64(time.Now().Add(expiration).Unix()),
			})
			err := <-errc
			log.Debugf("subnet udp send findvalue to node %v, key:%s, addpending err: %v", _node.addr(), common.Bytes2Hex(_key[:]), err)
		}(key, node)
	}
}

// toNodes is usually the result of lookup(targetid)
// key is subnet id
func (u *udp) store(key NodeID, value []byte, toNodes []*Node) {
	for _, node := range toNodes {
		log.Debugf("subnet udp send store to node: %v, value: %v", node, value)
		go func(_key NodeID, _value []byte, _node *Node) {
			u.send(
				_node.ID,
				_node.addr(),
				STOREPACKET,
				&store{
					Key:        _key,
					Value:      _value,
					From:       u.ourEndpoint,
					Expiration: uint64(time.Now().Add(expiration).Unix()),
				})
		}(key, value, node)
	}
}

// pending adds a reply callback to the pending reply queue.
// see the documentation of type pending for a detailed explanation.
func (u *udp) addPending(id NodeID, ptype byte, callback func(interface{}) bool) <-chan error {
	ch := make(chan error, 1)
	p := &pending{from: id, ptype: ptype, callback: callback, errc: ch}
	select {
	case u.pendings <- p: // loop() will call callback on the reply

	case <-u.closing:
		ch <- errClosed
	}
	return ch
}

func (u *udp) handleReply(from NodeID, ptype byte, req packet) bool {
	matched := make(chan bool, 1)
	select {
	case u.gotreply <- reply{from, ptype, req, matched}:
		// loop() will handle it and it will block on <-matched chan
		// until inside loop() send value into it
		return <-matched
	case <-u.closing:
		return false
	}
}

// loop runs in its own goroutine. it keeps track of
// the refresh timer and the pending reply queue.
func (u *udp) loop() {
	var (
		plist        = list.New()
		timeout      = time.NewTimer(0)
		nextTimeout  *pending // head of plist when timeout was last reset
		contTimeouts = 0      // number of continuous timeouts to do NTP checks
		ntpWarnTime  = time.Unix(0, 0)
	)
	<-timeout.C // ignore first timeout
	defer timeout.Stop()

	resetTimeout := func() {
		if plist.Front() == nil || nextTimeout == plist.Front().Value {
			return
		}
		// Start the timer so it fires when the next pending reply has expired.
		now := time.Now()
		for el := plist.Front(); el != nil; el = el.Next() {
			nextTimeout = el.Value.(*pending)
			if dist := nextTimeout.deadline.Sub(now); dist < 2*respTimeout {
				timeout.Reset(dist)
				return
			}
			// Remove pending replies whose deadline is too far in the
			// future. These can occur if the system clock jumped
			// backwards after the deadline was assigned.
			nextTimeout.errc <- errClockWarp
			plist.Remove(el)
			log.Debugf(
				"rpc pending removed deadline too far after %d ms [%d]",
				now.Sub(nextTimeout.createAt)/time.Millisecond,
				int(nextTimeout.ptype),
			)
		}
		nextTimeout = nil
		timeout.Stop()
	}

	for {
		resetTimeout()

		select {
		case <-u.closing:
			for el := plist.Front(); el != nil; el = el.Next() {
				el.Value.(*pending).errc <- errClosed
			}
			return

		case p := <-u.pendings:
			now := time.Now()
			p.deadline = now.Add(respTimeout)
			p.createAt = now
			plist.PushBack(p)

		case r := <-u.gotreply:
			var matched bool
			for el := plist.Front(); el != nil; el = el.Next() {
				p := el.Value.(*pending)
				if p.from == r.from && p.ptype == r.ptype {
					matched = true
					// Remove the matcher if its callback indicates
					// that all replies have been received. This is
					// required for packet types that expect multiple
					// reply packets.
					if p.callback(r.data) {
						p.errc <- nil
						plist.Remove(el)
						log.Debugf(
							"rpc pending got replay after %d ms [%d]",
							time.Now().Sub(p.createAt)/time.Millisecond,
							int(p.ptype),
						)
					}
					// Reset the continuous timeout counter (time drift detection)
					contTimeouts = 0
				}
			}
			r.matched <- matched

		case now := <-timeout.C:
			nextTimeout = nil

			// Notify and remove callbacks whose deadline is in the past.
			for el := plist.Front(); el != nil; el = el.Next() {
				p := el.Value.(*pending)
				if now.After(p.deadline) || now.Equal(p.deadline) {
					log.Debugf("rpc behind in %d ms", now.Sub(p.deadline)/time.Millisecond)
					p.errc <- errTimeout
					plist.Remove(el)
					log.Debugf(
						"rpc pending timeout after %d ms [%d]",
						now.Sub(p.createAt)/time.Millisecond,
						int(p.ptype),
					)
					contTimeouts++
				}
			}
			// If we've accumulated too many timeouts, do an NTP time sync check
			if contTimeouts > ntpFailureThreshold {
				if time.Since(ntpWarnTime) >= ntpWarningCooldown {
					ntpWarnTime = time.Now()
					go checkClockDrift()
				}
				contTimeouts = 0
			}
		}
	}
}

const (
	macSize  = 256 / 8
	sigSize  = 520 / 8
	headSize = macSize + sigSize // space of packet frame data
)

var (
	headSpace = make([]byte, headSize)

	// Neighbors replies are sent across multiple packets to
	// stay below the 1280 byte limit. We compute the maximum number
	// of entries by stuffing a packet until it grows too large.
	maxNeighbors int
)

func init() {
	p := neighbors{Expiration: ^uint64(0)}
	maxSizeNode := rpcNode{
		IP:  make(net.IP, 16),
		UDP: ^uint16(0),
		TCP: ^uint16(0),
		ID:  NodeID{},
	}
	for n := 0; ; n++ {
		p.Nodes = append(p.Nodes, maxSizeNode)
		size, _, err := rlp.EncodeToReader(p)
		if err != nil {
			// If this ever happens, it will be caught by the unit tests.
			panic("cannot encode: " + err.Error())
		}
		if headSize+size+1 >= 1280 {
			maxNeighbors = n
			log.Debugf("p2p udp max neighbors = %d", maxNeighbors)
			break
		}
	}
}

func (u *udp) send(toID NodeID, toaddr *net.UDPAddr, ptype byte, req packet) error {
	packet, err := encodePacket(u.priv, ptype, req)
	if err != nil {
		log.Debugf("error in encode udp packet: %s, %v", req.name(), err)
		return err
	}
	_, err = u.conn.WriteToUDP(packet, toaddr)
	log.Debug(">> "+req.name(), "addr", toaddr, "err", err, "id", toID.String()[:16])
	return err
}

func encodePacket(priv *ecdsa.PrivateKey, ptype byte, req interface{}) ([]byte, error) {
	b := new(bytes.Buffer)
	b.Write(headSpace)
	b.WriteByte(ptype)
	if err := rlp.Encode(b, req); err != nil {
		log.Error("Can't encode discv4 packet", "err", err)
		return nil, err
	}
	packet := b.Bytes()
	sig, err := crypto.Sign(crypto.Keccak256(packet[headSize:]), priv)
	if err != nil {
		log.Error("Can't sign discv4 packet", "err", err)
		return nil, err
	}
	copy(packet[macSize:], sig)
	// add the hash to the front. Note: this doesn't protect the
	// packet in any way. Our public key will be part of this hash in
	// The future.
	copy(packet, crypto.Keccak256(packet[macSize:]))
	return packet, nil
}

// readLoop runs in its own goroutine. it handles incoming UDP packets.
func (u *udp) readLoop() {
	defer u.conn.Close()
	// Discovery packets are defined to be no larger than 1280 bytes.
	// Packets larger than this size will be cut at the end and treated
	// as invalid because their hash won't match.
	buf := make([]byte, 1280)
	for {
		nbytes, from, err := u.conn.ReadFromUDP(buf)
		if netutil.IsTemporaryError(err) {
			// Ignore temporary read errors.
			log.Debug("Temporary UDP read error", "err", err)
			continue
		} else if err != nil {
			// Shut down the loop for permament errors.
			log.Debug("UDP read error", "err", err)
			return
		}
		u.handlePacket(from, buf[:nbytes])
	}
}

func (u *udp) handlePacket(from *net.UDPAddr, buf []byte) error {
	packet, fromID, hash, err := decodePacket(buf)

	// ignore any packet sent from alien node
	if u.Table.GetNodeType(fromID) == AlienNode {
		return errors.New("Node seen before as alien node")
	}

	if err != nil {
		log.Debug("Bad discv4 packet", "addr", from, "err", err)
		return err
	}

	// call different handle func base on the type of the packet
	err = packet.handle(u, from, fromID, hash)
	log.Trace("<< "+packet.name(), "addr", from, "err", err, "id", fromID.String()[:16])
	return err
}

func decodePacket(buf []byte) (packet, NodeID, []byte, error) {

	if len(buf) < headSize+1 {
		return nil, NodeID{}, nil, errPacketTooSmall
	}
	hash, sig, sigdata := buf[:macSize], buf[macSize:headSize], buf[headSize:]
	shouldhash := crypto.Keccak256(buf[macSize:])
	if !bytes.Equal(hash, shouldhash) {
		return nil, NodeID{}, nil, errBadHash
	}
	fromID, err := recoverNodeID(crypto.Keccak256(buf[headSize:]), sig)
	if err != nil {
		return nil, NodeID{}, hash, err
	}
	var req packet
	switch ptype := sigdata[0]; ptype {
	case PINGPACKET:
		req = new(ping)
	case PONGPACKET:
		req = new(pong)
	case FINDNODEPACKET:
		req = new(findnode)
	case NEIGHBORSPACKET:
		req = new(neighbors)
	case STOREPACKET:
		req = new(store)
	case STOREREPLYPACKET:
		req = new(storeReply)
	case FINDVALUEPACKET:
		req = new(findvalue)
	case FINDVALUEREPLYPACKET:
		req = new(findvalueReply)
	default:
		return nil, fromID, hash, fmt.Errorf("unknown type: %d", ptype)
	}
	s := rlp.NewStream(bytes.NewReader(sigdata[1:]), 0)
	err = s.Decode(req)
	return req, fromID, hash, err
}

// processRestInPingPong process the rest field in the ping/pong request
// and return the node type of the remote node
func processRestInPingPong(
	Rest []rlp.RawValue, u *udp, reqName string,
	from *net.UDPAddr, fromID NodeID,
) int {
	var rest string
	network_id := uint64(0)

	// extract network id from rest
	if len(Rest) > 0 {
		rest = string(Rest[0][1:])
		restList := strings.Split(string(rest), "\t")
		if len(restList) >= 1 {
			_network_id, err := strconv.Atoi(restList[0])
			if err == nil {
				network_id = uint64(_network_id)
			}
		}

		log.Debug(
			"<< "+reqName, "addr", from,
			"id", fromID.String()[:16], "network_id", network_id,
			"self network_id", u.networkid,
		)
	}

	remoteNodeType := UnknownNode
	// if remote network id is set and equals to ours, it's a brother node
	if network_id == u.networkid {
		remoteNodeType = BrotherNode
		// theoretically, we still need to check genesis
		// but usually if network ids are the same, so are genesis
		u.Table.SetNodeType(fromID, BrotherNode)
	} else {
		// if remote network id is set and it's different
		if network_id != 0 {
			remoteNodeType = AlienNode
			u.Table.SetNodeType(fromID, AlienNode)
		}
	}

	return remoteNodeType
}

func (req *ping) handle(u *udp, from *net.UDPAddr, fromID NodeID, mac []byte) error {
	if expired(req.Expiration) {
		return errExpired
	}

	// newer client should send 'network id' in 'rest' in ping msg
	remoteNodeType := processRestInPingPong(req.Rest, u, req.name(), from, fromID)

	// by default, send pong to all requests unless node check is strict
	// (this is the case for subnet) and it's not a brother.
	sendPong := true
	if u.strictNodeCheck && remoteNodeType != BrotherNode {
		sendPong = false
	}

	if sendPong {
		PongMsg, _ := rlp.EncodeToBytes(fmt.Sprintf("%d\t", u.networkid))
		PongRest := []rlp.RawValue{PongMsg}
		u.send(fromID, from, PONGPACKET, &pong{
			To:         makeEndpoint(from, req.From.TCP),
			ReplyTok:   mac,
			Expiration: uint64(time.Now().Add(expiration).Unix()),
			Rest:       PongRest,
		})
		if !u.handleReply(fromID, PINGPACKET, req) {
			// Note: we're ignoring the provided IP address in the packet right now
			go u.bond(true, fromID, from, req.From.TCP)
		}
	}

	return nil
}

func (req *ping) name() string { return "PING/v4" }

func (req *pong) handle(u *udp, from *net.UDPAddr, fromID NodeID, mac []byte) error {
	if expired(req.Expiration) {
		return errExpired
	}

	// newer client should reply 'network id' in 'rest' in pong msg
	processRestInPingPong(req.Rest, u, req.name(), from, fromID)

	if !u.handleReply(fromID, PONGPACKET, req) {
		return errUnsolicitedReply
	}
	return nil
}

func (req *pong) name() string { return "PONG/v4" }

// handle findnode request and reply with neighbors
func (req *findnode) handle(u *udp, from *net.UDPAddr, fromID NodeID, mac []byte) error {
	t1 := time.Now()
	if expired(req.Expiration) {
		return errExpired
	}
	if u.db.node(fromID) == nil {
		// No bond exists, we don't process the packet. This prevents
		// an attack vector where the discovery protocol could be used
		// to amplify traffic in a DDOS attack. A malicious actor
		// would send a findnode request with the IP address and UDP
		// port of the target as the source address. The recipient of
		// the findnode packet would then send a neighbors packet
		// (which is a much bigger packet than findnode) to the victim.
		return errUnknownNode
	}

	// by default, search all uncle and brother nodes, but if it is
	// strict node check, search only brother nodes. This is the case for
	// calling findnode before DHT STORE msg
	var rest string
	matchType := EitherUncleAndBrother
	// extract network id from rest
	if len(req.Rest) > 0 {
		rest = string(req.Rest[0][1:])
		strictNodeCheckInt, err := strconv.Atoi(rest)
		if err == nil {
			if strictNodeCheckInt == 1 {
				matchType = BrotherOnly
			}
		}
		log.Debug(
			"<< "+req.name(), "addr", from,
			"id", fromID.String()[:16], "strictNodeCheck", strictNodeCheckInt,
		)
	}

	target := crypto.Keccak256Hash(req.Target[:])
	u.mutex.Lock()
	closest := u.closest(
		target, bucketSize, matchType,
	).entries
	u.mutex.Unlock()

	p := neighbors{Expiration: uint64(time.Now().Add(expiration).Unix())}
	// Send neighbors in chunks with at most maxNeighbors per packet
	// to stay below the 1280 byte limit.
	for i, n := range closest {
		if netutil.CheckRelayIP(from.IP, n.IP) != nil {
			continue
		}
		p.Nodes = append(p.Nodes, nodeToRPC(n))
		// send neighbors in chunks of size maxNeighbors
		if len(p.Nodes) == maxNeighbors || i == len(closest)-1 {
			log.Debugf(
				"findnode handle took %.3f ms to finish",
				float64(time.Now().Sub(t1))/float64(time.Millisecond),
			)
			t2 := time.Now()
			u.send(fromID, from, NEIGHBORSPACKET, &p)
			log.Debugf(
				"findnode handle took %.3f ms to send",
				float64(time.Now().Sub(t2))/float64(time.Millisecond),
			)
			p.Nodes = p.Nodes[:0]
		}
	}
	return nil
}

func (req *findnode) name() string { return "FINDNODE/v4" }

func (req *neighbors) handle(u *udp, from *net.UDPAddr, fromID NodeID, mac []byte) error {
	if expired(req.Expiration) {
		return errExpired
	}
	if !u.handleReply(fromID, NEIGHBORSPACKET, req) {
		return errUnsolicitedReply
	}
	return nil
}

func (req *neighbors) name() string { return "NEIGHBORS/v4" }

// handle store request
func (req *store) handle(u *udp, from *net.UDPAddr, fromID NodeID, mac []byte) error {
	if expired(req.Expiration) {
		return errExpired
	}
	key := req.Key[:]
	node := u.db.node(fromID)

	// No bond exists, we don't process the packet.
	if node == nil {
		return errUnknownNode
	}
	// Example: "enode://8db......d74@172.20.0.13:40333"
	value := []byte(fmt.Sprintf("enode://%s@%s", fromID, from))
	log.Debugf("subnet store kv received from %v: %s", from, value)
	success := u.SetKey(key, value, fromID)
	p := storeReply{Expiration: uint64(time.Now().Add(expiration).Unix())}
	p.result = success
	u.send(fromID, from, STOREREPLYPACKET, &p)
	return nil
}

func (req *store) name() string { return "STORE/v4" }

// handle storereply request
func (req *storeReply) handle(u *udp, from *net.UDPAddr, fromID NodeID, mac []byte) error {
	if expired(req.Expiration) {
		return errExpired
	}
	log.Debugf("subnet store kv reply received from %v: %t", from, req.result)
	return nil
}

func (req *storeReply) name() string { return "STOREREPLY/v4" }

// handle findvalue request
func (req *findvalue) handle(u *udp, from *net.UDPAddr, fromID NodeID, mac []byte) error {
	if expired(req.Expiration) {
		return errExpired
	}

	reply := findvalueReply{Expiration: uint64(time.Now().Add(expiration).Unix())}
	// key is subnet id, value is a map[string]string
	value, _ := u.GetKey(req.Key[:])
	reply.Key = req.Key[:]
	reply.Value = []byte{}
	valueCount := len(value)

	var sb strings.Builder
	if valueCount == 0 {
		reply.Value = []byte("")
	} else if valueCount > 0 && valueCount <= params.SubnetBootNodeLimits {
		// return all the bootnodes we know
		for _, v := range value {
			sb.WriteString(v)
			sb.WriteString(",")
		}
		reply.Value = []byte(sb.String())
	} else if valueCount > params.SubnetBootNodeLimits {
		// shuffle the nodes and randomly get the first n bootnodes
		bootnodes := []string{}
		for _, v := range value {
			bootnodes = append(bootnodes, v)
		}
		rand.Seed(time.Now().UnixNano())
		rand.Shuffle(
			len(bootnodes),
			func(i, j int) { bootnodes[i], bootnodes[j] = bootnodes[j], bootnodes[i] },
		)
		for i := 0; i < params.SubnetBootNodeLimits; i++ {
			sb.WriteString(bootnodes[i])
			sb.WriteString(",")
		}
		reply.Value = []byte(sb.String())
	}

	log.Debugf(
		"subnet receive findvalue from %v, return for key %s with %s",
		common.Bytes2Hex(req.Key[:]), from, string(reply.Value),
	)
	u.send(fromID, from, FINDVALUEREPLYPACKET, &reply)
	return nil
}

func (req *findvalue) name() string { return "FINDVALUE/v4" }

// handle findvaluereply request
func (req *findvalueReply) handle(u *udp, from *net.UDPAddr, fromID NodeID, mac []byte) error {
	if expired(req.Expiration) {
		return errExpired
	}

	if !u.handleReply(fromID, FINDVALUEREPLYPACKET, req) {
		return errUnsolicitedReply
	}

	return nil
}

func (req *findvalueReply) name() string { return "FINDVALUEREPLY/v4" }

// helper function to check if expired
func expired(ts uint64) bool {
	return time.Unix(int64(ts), 0).Before(time.Now())
}
