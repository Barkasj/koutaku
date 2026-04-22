package clustering

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bytedance/sonic"
	"github.com/google/uuid"
)

// GossipNode implements the gossip protocol for cluster membership.
type GossipNode struct {
	mu sync.RWMutex

	// Local node information.
	localNode *Node
	config    ClusterConfig

	// Cluster state.
	nodes       map[string]*Node // node ID -> Node
	nodesByAddr map[string]*Node // address -> Node

	// Incarnation number (increments on restart/join).
	incarnation atomic.Uint64

	// Network transport.
	transport Transport
	listener  net.Listener

	// Channels.
	eventCh chan ClusterEvent
	stopCh  chan struct{}

	// WaitGroup for background goroutines.
	wg sync.WaitGroup

	// Discovery.
	discovery ServiceDiscovery

	// Statistics.
	startTime    time.Time
	msgSent      atomic.Uint64
	msgRecv      atomic.Uint64
	pingSent     atomic.Uint64
	pingRecv     atomic.Uint64
	ackSent      atomic.Uint64
	ackRecv      atomic.Uint64
	gossipCycles atomic.Uint64

	// Callbacks.
	onNodeJoin   func(*Node)
	onNodeLeave  func(*Node)
	onNodeFailed func(*Node)

	// Logger.
	logger *log.Logger
}

// NewGossipNode creates a new gossip node with the given configuration.
func NewGossipNode(config ClusterConfig, logger *log.Logger) (*GossipNode, error) {
	if logger == nil {
		logger = log.Default()
	}

	nodeName := config.NodeName
	if nodeName == "" {
		nodeName = getHostname()
	}

	nodeID := uuid.New().String()

	localNode := &Node{
		ID:          nodeID,
		Name:        nodeName,
		Address:     config.BindAddress,
		Metadata:    make(map[string]string),
		State:       NodeStateAlive,
		LastSeen:    time.Now(),
		Incarnation: 1,
	}

	gn := &GossipNode{
		localNode:   localNode,
		config:      config,
		nodes:       make(map[string]*Node),
		nodesByAddr: make(map[string]*Node),
		eventCh:     make(chan ClusterEvent, 100),
		stopCh:      make(chan struct{}),
		logger:      logger,
	}

	gn.incarnation.Store(1)

	return gn, nil
}

// Start starts the gossip node.
func (gn *GossipNode) Start(ctx context.Context) error {
	gn.mu.Lock()
	defer gn.mu.Unlock()

	gn.startTime = time.Now()

	// Add self to nodes map.
	gn.nodes[gn.localNode.ID] = gn.localNode
	gn.nodesByAddr[gn.localNode.Address] = gn.localNode

	// Start TCP listener.
	bindAddr := gn.config.BindAddress
	if bindAddr == "" {
		bindAddr = "0.0.0.0:10101"
	}

	var err error
	gn.listener, err = net.Listen("tcp", bindAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", bindAddr, err)
	}

	gn.logger.Printf("[Gossip] Node %s (%s) started on %s", gn.localNode.Name, gn.localNode.ID[:8], bindAddr)

	// Start background goroutines.
	gn.wg.Add(4)
	go gn.acceptLoop(ctx)
	go gn.probeLoop(ctx)
	go gn.gossipLoop(ctx)
	go gn.pushPullLoop(ctx)

	// Start discovery if configured.
	if gn.config.Discovery.Enabled {
		if err := gn.startDiscovery(ctx); err != nil {
			gn.logger.Printf("[Gossip] Warning: failed to start discovery: %v", err)
		}
	}

	// Join initial peers.
	if len(gn.config.Peers) > 0 {
		go gn.joinPeers(gn.config.Peers)
	}

	return nil
}

// Stop gracefully stops the gossip node.
func (gn *GossipNode) Stop() error {
	select {
	case <-gn.stopCh:
		return nil // already stopped
	default:
	}

	close(gn.stopCh)

	// Send leave message to all nodes.
	gn.sendLeave()

	// Stop discovery.
	if gn.discovery != nil {
		if err := gn.discovery.Stop(); err != nil {
			gn.logger.Printf("[Gossip] Error stopping discovery: %v", err)
		}
	}

	// Close listener to unblock Accept().
	if gn.listener != nil {
		gn.listener.Close()
	}

	// Wait for all goroutines to finish.
	gn.wg.Wait()

	gn.logger.Printf("[Gossip] Node %s stopped", gn.localNode.Name)
	return nil
}

// LocalNode returns the local node information.
func (gn *GossipNode) LocalNode() *Node {
	return gn.localNode
}

// Nodes returns all known nodes in the cluster.
func (gn *GossipNode) Nodes() []*Node {
	gn.mu.RLock()
	defer gn.mu.RUnlock()

	nodes := make([]*Node, 0, len(gn.nodes))
	for _, node := range gn.nodes {
		nodes = append(nodes, node)
	}
	return nodes
}

// AliveNodes returns only alive nodes.
func (gn *GossipNode) AliveNodes() []*Node {
	gn.mu.RLock()
	defer gn.mu.RUnlock()

	nodes := make([]*Node, 0, len(gn.nodes))
	for _, node := range gn.nodes {
		if node.State == NodeStateAlive {
			nodes = append(nodes, node)
		}
	}
	return nodes
}

// Join joins the cluster by connecting to the given peers.
func (gn *GossipNode) Join(peers []string) (int, error) {
	return gn.joinPeers(peers), nil
}

// Leave gracefully leaves the cluster.
func (gn *GossipNode) Leave() error {
	gn.sendLeave()
	gn.localNode.State = NodeStateLeft
	return nil
}

// Metrics returns cluster metrics.
func (gn *GossipNode) Metrics() ClusterMetrics {
	gn.mu.RLock()
	defer gn.mu.RUnlock()

	var alive, suspect, dead int
	for _, node := range gn.nodes {
		switch node.State {
		case NodeStateAlive:
			alive++
		case NodeStateSuspect:
			suspect++
		case NodeStateDead:
			dead++
		}
	}

	return ClusterMetrics{
		TotalNodes:   len(gn.nodes),
		AliveNodes:   alive,
		SuspectNodes: suspect,
		DeadNodes:    dead,
		Uptime:       time.Since(gn.startTime),
	}
}

// Events returns a channel that receives cluster events.
func (gn *GossipNode) Events() <-chan ClusterEvent {
	return gn.eventCh
}

// IsLeader returns true if this node is the cluster leader.
// Leader is the node with the lowest ID among alive nodes.
func (gn *GossipNode) IsLeader() bool {
	gn.mu.RLock()
	defer gn.mu.RUnlock()

	for _, node := range gn.nodes {
		if node.State == NodeStateAlive && node.ID < gn.localNode.ID {
			return false
		}
	}
	return true
}

// GetNode returns a specific node by ID.
func (gn *GossipNode) GetNode(id string) (*Node, bool) {
	gn.mu.RLock()
	defer gn.mu.RUnlock()

	node, ok := gn.nodes[id]
	return node, ok
}

// Broadcast sends a message to all alive nodes.
func (gn *GossipNode) Broadcast(msg *GossipMessage) error {
	gn.mu.RLock()
	nodes := make([]*Node, 0, len(gn.nodes))
	for _, node := range gn.nodes {
		if node.State == NodeStateAlive && node.ID != gn.localNode.ID {
			nodes = append(nodes, node)
		}
	}
	gn.mu.RUnlock()

	for _, node := range nodes {
		if err := gn.sendToNode(node, msg); err != nil {
			gn.logger.Printf("[Gossip] Failed to send to %s: %v", node.Name, err)
		}
	}
	return nil
}

// Send sends a message to a specific node.
func (gn *GossipNode) Send(nodeID string, msg *GossipMessage) error {
	gn.mu.RLock()
	node, ok := gn.nodes[nodeID]
	gn.mu.RUnlock()

	if !ok {
		return fmt.Errorf("node %s not found", nodeID)
	}

	return gn.sendToNode(node, msg)
}

// NumMembers returns the number of alive members.
func (gn *GossipNode) NumMembers() int {
	gn.mu.RLock()
	defer gn.mu.RUnlock()

	count := 0
	for _, node := range gn.nodes {
		if node.State == NodeStateAlive {
			count++
		}
	}
	return count
}

// --- Internal methods ---

// acceptLoop accepts incoming TCP connections.
func (gn *GossipNode) acceptLoop(ctx context.Context) {
	defer gn.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case <-gn.stopCh:
			return
		default:
		}

		conn, err := gn.listener.Accept()
		if err != nil {
			select {
			case <-gn.stopCh:
				return
			default:
				gn.logger.Printf("[Gossip] Accept error: %v", err)
				continue
			}
		}

		go gn.handleConn(ctx, conn)
	}
}

// handleConn handles an incoming connection.
func (gn *GossipNode) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	// Set read deadline.
	conn.SetReadDeadline(time.Now().Add(gn.config.Gossip.Config.TCPTimeout))

	// Read message length.
	var msgLen uint32
	if err := binary.Read(conn, binary.BigEndian, &msgLen); err != nil {
		return
	}

	// Read message data.
	msgData := make([]byte, msgLen)
	if _, err := io.ReadFull(conn, msgData); err != nil {
		return
	}

	// Parse message.
	var msg GossipMessage
	if err := sonic.Unmarshal(msgData, &msg); err != nil {
		return
	}

	gn.msgRecv.Add(1)

	// Handle message based on type.
	switch msg.Type {
	case MsgPing:
		gn.handlePing(conn, &msg)
	case MsgPingReq:
		gn.handlePingReq(conn, &msg)
	case MsgAck:
		gn.handleAck(&msg)
	case MsgJoin:
		gn.handleJoin(&msg)
	case MsgLeave:
		gn.handleLeave(&msg)
	case MsgAlive:
		gn.handleAlive(&msg)
	case MsgSuspect:
		gn.handleSuspect(&msg)
	case MsgDead:
		gn.handleDead(&msg)
	case MsgPushPull:
		gn.handlePushPull(conn, &msg)
	}
}

// handlePing handles a ping message.
func (gn *GossipNode) handlePing(conn net.Conn, msg *GossipMessage) {
	gn.pingRecv.Add(1)

	// Update sender's state.
	gn.mergeNode(msg.Sender)

	// Send ack.
	ack := &GossipMessage{
		Type:        MsgAck,
		Sender:      gn.localNode,
		Incarnation: gn.incarnation.Load(),
	}

	gn.sendMsg(conn, ack)
	gn.ackSent.Add(1)
}

// handlePingReq handles an indirect ping request.
func (gn *GossipNode) handlePingReq(conn net.Conn, msg *GossipMessage) {
	// Ping the target node on behalf of the requester.
	if len(msg.Nodes) > 0 {
		target := msg.Nodes[0]
		if err := gn.pingNode(target); err != nil {
			// Target is unreachable, send suspect.
			suspect := &GossipMessage{
				Type:        MsgSuspect,
				Sender:      gn.localNode,
				Nodes:       []*Node{target},
				Incarnation: gn.incarnation.Load(),
			}
			gn.sendMsg(conn, suspect)
		} else {
			// Target is alive, send ack.
			ack := &GossipMessage{
				Type:        MsgAck,
				Sender:      target,
				Incarnation: target.Incarnation,
			}
			gn.sendMsg(conn, ack)
		}
	}
}

// handleAck handles an acknowledgment message.
func (gn *GossipNode) handleAck(msg *GossipMessage) {
	gn.ackRecv.Add(1)
	gn.mergeNode(msg.Sender)
}

// handleJoin handles a join message.
func (gn *GossipNode) handleJoin(msg *GossipMessage) {
	gn.mergeNode(msg.Sender)

	gn.eventCh <- ClusterEvent{
		Type:      EventNodeJoin,
		Node:      msg.Sender,
		Timestamp: time.Now(),
	}

	gn.logger.Printf("[Gossip] Node %s joined the cluster", msg.Sender.Name)
}

// handleLeave handles a leave message.
func (gn *GossipNode) handleLeave(msg *GossipMessage) {
	gn.mu.Lock()
	if node, ok := gn.nodes[msg.Sender.ID]; ok {
		node.State = NodeStateLeft
		node.LastSeen = time.Now()
	}
	gn.mu.Unlock()

	gn.eventCh <- ClusterEvent{
		Type:      EventNodeLeave,
		Node:      msg.Sender,
		Timestamp: time.Now(),
	}

	gn.logger.Printf("[Gossip] Node %s left the cluster", msg.Sender.Name)
}

// handleAlive handles an alive message.
func (gn *GossipNode) handleAlive(msg *GossipMessage) {
	gn.mergeNode(msg.Sender)
}

// handleSuspect handles a suspect message.
func (gn *GossipNode) handleSuspect(msg *GossipMessage) {
	gn.mu.Lock()
	for _, suspectNode := range msg.Nodes {
		if node, ok := gn.nodes[suspectNode.ID]; ok {
			if node.ID == gn.localNode.ID {
				// Someone suspects us, increment incarnation to prove we're alive.
				gn.incarnation.Add(1)
				gn.localNode.Incarnation = gn.incarnation.Load()
				gn.localNode.LastSeen = time.Now()

				// Broadcast alive message.
				alive := &GossipMessage{
					Type:        MsgAlive,
					Sender:      gn.localNode,
					Incarnation: gn.incarnation.Load(),
				}
				gn.Broadcast(alive)
			} else if node.State == NodeStateAlive {
				node.State = NodeStateSuspect
				node.LastSeen = time.Now()

				gn.eventCh <- ClusterEvent{
					Type:      EventNodeUpdate,
					Node:      node,
					Timestamp: time.Now(),
					Metadata:  map[string]string{"state": "suspect"},
				}
			}
		}
	}
	gn.mu.Unlock()
}

// handleDead handles a dead message.
func (gn *GossipNode) handleDead(msg *GossipMessage) {
	gn.mu.Lock()
	for _, deadNode := range msg.Nodes {
		if node, ok := gn.nodes[deadNode.ID]; ok {
			node.State = NodeStateDead
			node.LastSeen = time.Now()

			gn.eventCh <- ClusterEvent{
				Type:      EventNodeFailed,
				Node:      node,
				Timestamp: time.Now(),
			}

			gn.logger.Printf("[Gossip] Node %s is dead", node.Name)
		}
	}
	gn.mu.Unlock()
}

// handlePushPull handles a full state synchronization.
func (gn *GossipNode) handlePushPull(conn net.Conn, msg *GossipMessage) {
	// Merge received nodes.
	for _, remoteNode := range msg.Nodes {
		gn.mergeNode(remoteNode)
	}

	// Send our state back.
	response := &GossipMessage{
		Type:        MsgPushPull,
		Sender:      gn.localNode,
		Nodes:       gn.Nodes(),
		Incarnation: gn.incarnation.Load(),
	}

	gn.sendMsg(conn, response)
}

// probeLoop periodically probes random nodes for health.
func (gn *GossipNode) probeLoop(ctx context.Context) {
	defer gn.wg.Done()

	probeInterval := gn.config.Gossip.Config.ProbeInterval
	if probeInterval <= 0 {
		probeInterval = 1 * time.Second
	}

	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-gn.stopCh:
			return
		case <-ticker.C:
			gn.probeRandomNode()
		}
	}
}

// probeRandomNode selects a random alive node and pings it.
func (gn *GossipNode) probeRandomNode() {
	gn.mu.RLock()
	var candidates []*Node
	for _, node := range gn.nodes {
		if node.State == NodeStateAlive && node.ID != gn.localNode.ID {
			candidates = append(candidates, node)
		}
	}
	gn.mu.RUnlock()

	if len(candidates) == 0 {
		return
	}

	// Select random node.
	target := candidates[rand.Intn(len(candidates))]

	if err := gn.pingNode(target); err != nil {
		// Node is unreachable, mark as suspect.
		gn.mu.Lock()
		if node, ok := gn.nodes[target.ID]; ok && node.State == NodeStateAlive {
			node.State = NodeStateSuspect
			node.LastSeen = time.Now()

			gn.eventCh <- ClusterEvent{
				Type:      EventNodeUpdate,
				Node:      node,
				Timestamp: time.Now(),
				Metadata:  map[string]string{"state": "suspect"},
			}

			// Indirect ping via other nodes.
			gn.indirectPing(node)
		}
		gn.mu.Unlock()
	} else {
		// Node is alive.
		gn.mu.Lock()
		if node, ok := gn.nodes[target.ID]; ok {
			node.State = NodeStateAlive
			node.LastSeen = time.Now()
		}
		gn.mu.Unlock()
	}
}

// pingNode sends a ping to a node and waits for ack.
func (gn *GossipNode) pingNode(target *Node) error {
	probeTimeout := gn.config.Gossip.Config.ProbeTimeout
	if probeTimeout <= 0 {
		probeTimeout = 500 * time.Millisecond
	}

	conn, err := net.DialTimeout("tcp", target.Address, probeTimeout)
	if err != nil {
		return err
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(probeTimeout))

	// Send ping.
	ping := &GossipMessage{
		Type:        MsgPing,
		Sender:      gn.localNode,
		Incarnation: gn.incarnation.Load(),
	}

	if err := gn.sendMsg(conn, ping); err != nil {
		return err
	}
	gn.pingSent.Add(1)

	// Read ack.
	var msgLen uint32
	if err := binary.Read(conn, binary.BigEndian, &msgLen); err != nil {
		return err
	}

	msgData := make([]byte, msgLen)
	if _, err := io.ReadFull(conn, msgData); err != nil {
		return err
	}

	var ack GossipMessage
	if err := sonic.Unmarshal(msgData, &ack); err != nil {
		return err
	}

	if ack.Type != MsgAck {
		return fmt.Errorf("expected ack, got %s", ack.Type)
	}

	gn.ackRecv.Add(1)
	return nil
}

// indirectPing attempts to ping a node through other alive nodes.
func (gn *GossipNode) indirectPing(target *Node) {
	failureThreshold := gn.config.Gossip.Config.FailureThreshold
	if failureThreshold <= 0 {
		failureThreshold = 3
	}

	// Select random nodes for indirect ping.
	gn.mu.RLock()
	var relayNodes []*Node
	for _, node := range gn.nodes {
		if node.State == NodeStateAlive && node.ID != gn.localNode.ID && node.ID != target.ID {
			relayNodes = append(relayNodes, node)
		}
	}
	gn.mu.RUnlock()

	// Use up to 3 relay nodes.
	relayCount := 3
	if len(relayNodes) < relayCount {
		relayCount = len(relayNodes)
	}

	for i := 0; i < relayCount; i++ {
		relay := relayNodes[rand.Intn(len(relayNodes))]
		pingReq := &GossipMessage{
			Type:        MsgPingReq,
			Sender:      gn.localNode,
			Nodes:       []*Node{target},
			Incarnation: gn.incarnation.Load(),
		}

		if err := gn.sendToNode(relay, pingReq); err != nil {
			gn.logger.Printf("[Gossip] Failed to send ping_req to %s: %v", relay.Name, err)
		}
	}
}

// gossipLoop periodically gossips with random nodes.
func (gn *GossipNode) gossipLoop(ctx context.Context) {
	defer gn.wg.Done()

	gossipInterval := gn.config.Gossip.Config.GossipInterval
	if gossipInterval <= 0 {
		gossipInterval = 200 * time.Millisecond
	}

	ticker := time.NewTicker(gossipInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-gn.stopCh:
			return
		case <-ticker.C:
			gn.gossipRandomNodes()
		}
	}
}

// gossipRandomNodes selects random nodes and sends gossip messages.
func (gn *GossipNode) gossipRandomNodes() {
	gossipNodes := gn.config.Gossip.Config.GossipNodes
	if gossipNodes <= 0 {
		gossipNodes = 3
	}

	gn.mu.RLock()
	var candidates []*Node
	for _, node := range gn.nodes {
		if node.State == NodeStateAlive && node.ID != gn.localNode.ID {
			candidates = append(candidates, node)
		}
	}
	gn.mu.RUnlock()

	if len(candidates) == 0 {
		return
	}

	// Select random nodes.
	n := gossipNodes
	if len(candidates) < n {
		n = len(candidates)
	}

	rand.Shuffle(len(candidates), func(i, j int) {
		candidates[i], candidates[j] = candidates[j], candidates[i]
	})

	// Send alive messages to selected nodes.
	alive := &GossipMessage{
		Type:        MsgAlive,
		Sender:      gn.localNode,
		Incarnation: gn.incarnation.Load(),
	}

	for i := 0; i < n; i++ {
		if err := gn.sendToNode(candidates[i], alive); err != nil {
			gn.logger.Printf("[Gossip] Failed to gossip with %s: %v", candidates[i].Name, err)
		}
	}

	gn.gossipCycles.Add(1)
}

// pushPullLoop periodically performs full state synchronization.
func (gn *GossipNode) pushPullLoop(ctx context.Context) {
	defer gn.wg.Done()

	pushPullInterval := gn.config.Gossip.Config.PushPullInterval
	if pushPullInterval <= 0 {
		pushPullInterval = 30 * time.Second
	}

	ticker := time.NewTicker(pushPullInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-gn.stopCh:
			return
		case <-ticker.C:
			gn.pushPullRandomNode()
		}
	}
}

// pushPullRandomNode performs a full state sync with a random node.
func (gn *GossipNode) pushPullRandomNode() {
	gn.mu.RLock()
	var candidates []*Node
	for _, node := range gn.nodes {
		if node.State == NodeStateAlive && node.ID != gn.localNode.ID {
			candidates = append(candidates, node)
		}
	}
	gn.mu.RUnlock()

	if len(candidates) == 0 {
		return
	}

	target := candidates[rand.Intn(len(candidates))]

	tcpTimeout := gn.config.Gossip.Config.TCPTimeout
	if tcpTimeout <= 0 {
		tcpTimeout = 10 * time.Second
	}

	conn, err := net.DialTimeout("tcp", target.Address, tcpTimeout)
	if err != nil {
		gn.logger.Printf("[Gossip] PushPull failed to connect to %s: %v", target.Name, err)
		return
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(tcpTimeout))

	// Send our state.
	pushPull := &GossipMessage{
		Type:        MsgPushPull,
		Sender:      gn.localNode,
		Nodes:       gn.Nodes(),
		Incarnation: gn.incarnation.Load(),
	}

	if err := gn.sendMsg(conn, pushPull); err != nil {
		gn.logger.Printf("[Gossip] PushPull failed to send to %s: %v", target.Name, err)
		return
	}

	// Read response.
	var msgLen uint32
	if err := binary.Read(conn, binary.BigEndian, &msgLen); err != nil {
		return
	}

	msgData := make([]byte, msgLen)
	if _, err := io.ReadFull(conn, msgData); err != nil {
		return
	}

	var response GossipMessage
	if err := sonic.Unmarshal(msgData, &response); err != nil {
		return
	}

	// Merge received nodes.
	for _, remoteNode := range response.Nodes {
		gn.mergeNode(remoteNode)
	}

	gn.eventCh <- ClusterEvent{
		Type:      EventStateSync,
		Node:      target,
		Timestamp: time.Now(),
	}
}

// mergeNode merges a remote node into our local state.
func (gn *GossipNode) mergeNode(remoteNode *Node) {
	if remoteNode == nil || remoteNode.ID == gn.localNode.ID {
		return
	}

	gn.mu.Lock()
	defer gn.mu.Unlock()

	localNode, exists := gn.nodes[remoteNode.ID]
	if !exists {
		// New node.
		gn.nodes[remoteNode.ID] = remoteNode
		gn.nodesByAddr[remoteNode.Address] = remoteNode

		gn.eventCh <- ClusterEvent{
			Type:      EventNodeJoin,
			Node:      remoteNode,
			Timestamp: time.Now(),
		}

		gn.logger.Printf("[Gossip] Discovered new node %s (%s)", remoteNode.Name, remoteNode.Address)
		return
	}

	// Update existing node using incarnation-based conflict resolution.
	if remoteNode.Incarnation > localNode.Incarnation {
		localNode.State = remoteNode.State
		localNode.LastSeen = remoteNode.LastSeen
		localNode.Incarnation = remoteNode.Incarnation
		localNode.Metadata = remoteNode.Metadata

		gn.eventCh <- ClusterEvent{
			Type:      EventNodeUpdate,
			Node:      localNode,
			Timestamp: time.Now(),
		}
	} else if remoteNode.Incarnation == localNode.Incarnation {
		// Same incarnation, update LastSeen if remote is newer.
		if remoteNode.LastSeen.After(localNode.LastSeen) {
			localNode.LastSeen = remoteNode.LastSeen
			if remoteNode.State == NodeStateAlive && localNode.State == NodeStateSuspect {
				localNode.State = NodeStateAlive
			}
		}
	}
}

// joinPeers joins the cluster by connecting to the given peers.
func (gn *GossipNode) joinPeers(peers []string) int {
	joined := 0

	for _, peer := range peers {
		conn, err := net.DialTimeout("tcp", peer, 5*time.Second)
		if err != nil {
			gn.logger.Printf("[Gossip] Failed to connect to peer %s: %v", peer, err)
			continue
		}

		join := &GossipMessage{
			Type:        MsgJoin,
			Sender:      gn.localNode,
			Incarnation: gn.incarnation.Load(),
		}

		if err := gn.sendMsg(conn, join); err != nil {
			gn.logger.Printf("[Gossip] Failed to send join to %s: %v", peer, err)
			conn.Close()
			continue
		}

		conn.Close()
		joined++
		gn.logger.Printf("[Gossip] Joined peer %s", peer)
	}

	return joined
}

// sendLeave sends a leave message to all nodes.
func (gn *GossipNode) sendLeave() {
	leave := &GossipMessage{
		Type:        MsgLeave,
		Sender:      gn.localNode,
		Incarnation: gn.incarnation.Load(),
	}

	// Send asynchronously with a short timeout to avoid blocking shutdown.
	gn.mu.RLock()
	nodes := make([]*Node, 0, len(gn.nodes))
	for _, node := range gn.nodes {
		if node.State == NodeStateAlive && node.ID != gn.localNode.ID {
			nodes = append(nodes, node)
		}
	}
	gn.mu.RUnlock()

	for _, node := range nodes {
		go func(n *Node) {
			shortTimeout := 500 * time.Millisecond
			conn, err := net.DialTimeout("tcp", n.Address, shortTimeout)
			if err != nil {
				return
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(shortTimeout))
			gn.sendMsg(conn, leave)
		}(node)
	}
}

// sendToNode sends a message to a specific node.
func (gn *GossipNode) sendToNode(node *Node, msg *GossipMessage) error {
	tcpTimeout := gn.config.Gossip.Config.TCPTimeout
	if tcpTimeout <= 0 {
		tcpTimeout = 10 * time.Second
	}

	conn, err := net.DialTimeout("tcp", node.Address, tcpTimeout)
	if err != nil {
		return err
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(tcpTimeout))

	return gn.sendMsg(conn, msg)
}

// sendMsg sends a message over a connection.
func (gn *GossipNode) sendMsg(conn net.Conn, msg *GossipMessage) error {
	data, err := sonic.Marshal(msg)
	if err != nil {
		return err
	}

	// Write length prefix.
	msgLen := uint32(len(data))
	if err := binary.Write(conn, binary.BigEndian, msgLen); err != nil {
		return err
	}

	// Write message data.
	_, err = conn.Write(data)
	if err != nil {
		return err
	}

	gn.msgSent.Add(1)
	return nil
}

// startDiscovery starts the service discovery.
func (gn *GossipNode) startDiscovery(ctx context.Context) error {
	discoveryType := gn.config.Discovery.Type

	switch discoveryType {
	case "kubernetes":
		gn.discovery = NewKubernetesDiscovery(gn.config.Discovery)
	case "consul":
		gn.discovery = NewConsulDiscovery(gn.config.Discovery)
	case "etcd":
		gn.discovery = NewEtcdDiscovery(gn.config.Discovery)
	case "dns":
		gn.discovery = NewDNSDiscovery(gn.config.Discovery)
	case "udp":
		gn.discovery = NewUDPDiscovery(gn.config.Discovery)
	case "mdns":
		gn.discovery = NewMDNSDiscovery(gn.config.Discovery)
	default:
		return fmt.Errorf("unsupported discovery type: %s", discoveryType)
	}

	if err := gn.discovery.Start(ctx); err != nil {
		return err
	}

	// Watch for discovery events.
	events, err := gn.discovery.Watch()
	if err != nil {
		return err
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-gn.stopCh:
				return
			case event, ok := <-events:
				if !ok {
					return
				}
				switch event.Type {
				case DiscoveryNodeFound:
					gn.mergeNode(event.Node)
				case DiscoveryNodeLost:
					gn.mu.Lock()
					if node, ok := gn.nodes[event.Node.ID]; ok {
						node.State = NodeStateDead
						node.LastSeen = time.Now()
					}
					gn.mu.Unlock()
				}
			}
		}
	}()

	return nil
}

// getHostname returns the hostname of the machine.
func getHostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		return "koutaku-" + uuid.New().String()[:8]
	}
	return hostname
}
