package clustering

import (
	"context"
	"log"
	"os"
	"testing"
	"time"
)

func TestNewGossipNode(t *testing.T) {
	config := DefaultClusterConfig()
	config.NodeName = "test-node-1"
	config.BindAddress = "127.0.0.1:10101"

	logger := log.New(os.Stdout, "[Test] ", log.LstdFlags)

	node, err := NewGossipNode(config, logger)
	if err != nil {
		t.Fatalf("Failed to create gossip node: %v", err)
	}

	if node == nil {
		t.Fatal("Expected non-nil node")
	}

	if node.LocalNode().Name != "test-node-1" {
		t.Errorf("Expected node name 'test-node-1', got '%s'", node.LocalNode().Name)
	}

	if node.LocalNode().State != NodeStateAlive {
		t.Errorf("Expected node state 'alive', got '%s'", node.LocalNode().State)
	}
}

func TestGossipNodeStartStop(t *testing.T) {
	config := DefaultClusterConfig()
	config.NodeName = "test-node-2"
	config.BindAddress = "127.0.0.1:10102"

	logger := log.New(os.Stdout, "[Test] ", log.LstdFlags)

	node, err := NewGossipNode(config, logger)
	if err != nil {
		t.Fatalf("Failed to create gossip node: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := node.Start(ctx); err != nil {
		t.Fatalf("Failed to start gossip node: %v", err)
	}

	// Wait a bit for the node to start.
	time.Sleep(100 * time.Millisecond)

	// Check that the node is running.
	if node.LocalNode().State != NodeStateAlive {
		t.Errorf("Expected node state 'alive', got '%s'", node.LocalNode().State)
	}

	// Stop the node.
	if err := node.Stop(); err != nil {
		t.Fatalf("Failed to stop gossip node: %v", err)
	}
}

func TestGossipNodeMetrics(t *testing.T) {
	config := DefaultClusterConfig()
	config.NodeName = "test-node-3"
	config.BindAddress = "127.0.0.1:10103"

	logger := log.New(os.Stdout, "[Test] ", log.LstdFlags)

	node, err := NewGossipNode(config, logger)
	if err != nil {
		t.Fatalf("Failed to create gossip node: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := node.Start(ctx); err != nil {
		t.Fatalf("Failed to start gossip node: %v", err)
	}
	defer node.Stop()

	metrics := node.Metrics()
	if metrics.TotalNodes != 1 {
		t.Errorf("Expected 1 total node, got %d", metrics.TotalNodes)
	}

	if metrics.AliveNodes != 1 {
		t.Errorf("Expected 1 alive node, got %d", metrics.AliveNodes)
	}

	if metrics.Uptime <= 0 {
		t.Error("Expected positive uptime")
	}
}

func TestGossipNodeJoin(t *testing.T) {
	// Create two nodes.
	config1 := DefaultClusterConfig()
	config1.NodeName = "node-1"
	config1.BindAddress = "127.0.0.1:10104"
	config1.Gossip.Config.GossipInterval = 5 * time.Second // slow down gossip for test

	config2 := DefaultClusterConfig()
	config2.NodeName = "node-2"
	config2.BindAddress = "127.0.0.1:10105"
	config2.Peers = []string{"127.0.0.1:10104"}
	config2.Gossip.Config.GossipInterval = 5 * time.Second

	logger := log.New(os.Stdout, "[Test] ", log.LstdFlags)

	node1, err := NewGossipNode(config1, logger)
	if err != nil {
		t.Fatalf("Failed to create node1: %v", err)
	}

	node2, err := NewGossipNode(config2, logger)
	if err != nil {
		t.Fatalf("Failed to create node2: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Start node1 first.
	if err := node1.Start(ctx); err != nil {
		t.Fatalf("Failed to start node1: %v", err)
	}

	// Start node2 (will try to join node1).
	if err := node2.Start(ctx); err != nil {
		t.Fatalf("Failed to start node2: %v", err)
	}

	// Wait for nodes to discover each other.
	time.Sleep(2 * time.Second)

	// Check that both nodes see each other.
	if node1.NumMembers() < 2 {
		t.Errorf("Expected node1 to see at least 2 members, got %d", node1.NumMembers())
	}

	if node2.NumMembers() < 2 {
		t.Errorf("Expected node2 to see at least 2 members, got %d", node2.NumMembers())
	}

	// Stop both nodes.
	node1.Stop()
	node2.Stop()
}

func TestGossipNodeIsLeader(t *testing.T) {
	config := DefaultClusterConfig()
	config.NodeName = "leader-node"
	config.BindAddress = "127.0.0.1:10106"

	logger := log.New(os.Stdout, "[Test] ", log.LstdFlags)

	node, err := NewGossipNode(config, logger)
	if err != nil {
		t.Fatalf("Failed to create gossip node: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := node.Start(ctx); err != nil {
		t.Fatalf("Failed to start gossip node: %v", err)
	}
	defer node.Stop()

	// Single node should be leader.
	if !node.IsLeader() {
		t.Error("Expected single node to be leader")
	}
}

func TestGossipNodeEvents(t *testing.T) {
	config := DefaultClusterConfig()
	config.NodeName = "event-node"
	config.BindAddress = "127.0.0.1:10107"

	logger := log.New(os.Stdout, "[Test] ", log.LstdFlags)

	node, err := NewGossipNode(config, logger)
	if err != nil {
		t.Fatalf("Failed to create gossip node: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := node.Start(ctx); err != nil {
		t.Fatalf("Failed to start gossip node: %v", err)
	}
	defer node.Stop()

	// Check that events channel is not nil.
	events := node.Events()
	if events == nil {
		t.Error("Expected non-nil events channel")
	}
}

func TestGossipNodeGetNode(t *testing.T) {
	config := DefaultClusterConfig()
	config.NodeName = "get-node"
	config.BindAddress = "127.0.0.1:10108"

	logger := log.New(os.Stdout, "[Test] ", log.LstdFlags)

	node, err := NewGossipNode(config, logger)
	if err != nil {
		t.Fatalf("Failed to create gossip node: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := node.Start(ctx); err != nil {
		t.Fatalf("Failed to start gossip node: %v", err)
	}
	defer node.Stop()

	// Get local node.
	localNode := node.LocalNode()
	foundNode, ok := node.GetNode(localNode.ID)
	if !ok {
		t.Error("Expected to find local node")
	}

	if foundNode.ID != localNode.ID {
		t.Errorf("Expected node ID '%s', got '%s'", localNode.ID, foundNode.ID)
	}
}

func TestDefaultClusterConfig(t *testing.T) {
	config := DefaultClusterConfig()

	if config.Enabled {
		t.Error("Expected cluster to be disabled by default")
	}

	if config.BindAddress != "0.0.0.0:10101" {
		t.Errorf("Expected bind address '0.0.0.0:10101', got '%s'", config.BindAddress)
	}

	if config.Gossip.Port != 10101 {
		t.Errorf("Expected gossip port 10101, got %d", config.Gossip.Port)
	}

	if config.Gossip.Config.TimeoutSeconds != 10 {
		t.Errorf("Expected timeout 10 seconds, got %d", config.Gossip.Config.TimeoutSeconds)
	}

	if config.Gossip.Config.SuccessThreshold != 3 {
		t.Errorf("Expected success threshold 3, got %d", config.Gossip.Config.SuccessThreshold)
	}

	if config.Gossip.Config.FailureThreshold != 3 {
		t.Errorf("Expected failure threshold 3, got %d", config.Gossip.Config.FailureThreshold)
	}
}

func TestNodeStates(t *testing.T) {
	tests := []struct {
		state    NodeState
		expected string
	}{
		{NodeStateAlive, "alive"},
		{NodeStateSuspect, "suspect"},
		{NodeStateDead, "dead"},
		{NodeStateLeft, "left"},
	}

	for _, tt := range tests {
		if tt.state.String() != tt.expected {
			t.Errorf("Expected state '%s', got '%s'", tt.expected, tt.state.String())
		}
	}
}

func TestClusterEventTypes(t *testing.T) {
	tests := []struct {
		eventType ClusterEventType
		expected  string
	}{
		{EventNodeJoin, "node_join"},
		{EventNodeLeave, "node_leave"},
		{EventNodeFailed, "node_failed"},
		{EventNodeUpdate, "node_update"},
		{EventStateSync, "state_sync"},
	}

	for _, tt := range tests {
		if tt.eventType.String() != tt.expected {
			t.Errorf("Expected event type '%s', got '%s'", tt.expected, tt.eventType.String())
		}
	}
}

func TestGossipMessageTypes(t *testing.T) {
	tests := []struct {
		msgType  GossipMessageType
		expected string
	}{
		{MsgPing, "ping"},
		{MsgPingReq, "ping_req"},
		{MsgAck, "ack"},
		{MsgJoin, "join"},
		{MsgLeave, "leave"},
		{MsgAlive, "alive"},
		{MsgSuspect, "suspect"},
		{MsgDead, "dead"},
		{MsgPushPull, "push_pull"},
	}

	for _, tt := range tests {
		if tt.msgType.String() != tt.expected {
			t.Errorf("Expected message type '%s', got '%s'", tt.expected, tt.msgType.String())
		}
	}
}
