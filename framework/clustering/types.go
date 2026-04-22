// Package clustering provides enterprise-grade high-availability clustering
// with automatic service discovery, gossip-based state synchronization,
// and intelligent traffic distribution for Koutaku AI Gateway.
package clustering

import (
	"context"
	"net"
	"time"
)

// NodeState represents the health state of a cluster node.
type NodeState int

const (
	// NodeStateAlive indicates the node is healthy and responsive.
	NodeStateAlive NodeState = iota
	// NodeStateSuspect indicates the node is being health-checked.
	NodeStateSuspect
	// NodeStateDead indicates the node is unresponsive and removed from rotation.
	NodeStateDead
	// NodeStateLeft indicates the node gracefully left the cluster.
	NodeStateLeft
)

// String returns the string representation of NodeState.
func (s NodeState) String() string {
	switch s {
	case NodeStateAlive:
		return "alive"
	case NodeStateSuspect:
		return "suspect"
	case NodeStateDead:
		return "dead"
	case NodeStateLeft:
		return "left"
	default:
		return "unknown"
	}
}

// Node represents a member of the cluster.
type Node struct {
	// ID is the unique identifier for this node (UUID).
	ID string `json:"id"`
	// Name is a human-readable name for the node.
	Name string `json:"name"`
	// Address is the gossip protocol address (host:port).
	Address string `json:"address"`
	// Metadata contains arbitrary key-value pairs for the node.
	Metadata map[string]string `json:"metadata,omitempty"`
	// State is the current health state of the node.
	State NodeState `json:"state"`
	// LastSeen is the last time this node was seen alive.
	LastSeen time.Time `json:"last_seen"`
	// Incarnation is the node's incarnation number (increments on restart).
	Incarnation uint64 `json:"incarnation"`
}

// IsAlive returns true if the node is in alive state.
func (n *Node) IsAlive() bool {
	return n.State == NodeStateAlive
}

// ClusterConfig represents the configuration for cluster mode.
type ClusterConfig struct {
	// Enabled enables cluster mode.
	Enabled bool `json:"enabled"`
	// NodeName is the human-readable name for this node.
	// Defaults to hostname if empty.
	NodeName string `json:"node_name,omitempty"`
	// BindAddress is the address to bind the gossip protocol to.
	// Defaults to "0.0.0.0:10101".
	BindAddress string `json:"bind_address,omitempty"`
	// AdvertiseAddress is the address to advertise to other nodes.
	// Useful for NAT or Docker environments.
	AdvertiseAddress string `json:"advertise_address,omitempty"`
	// Discovery configures how nodes discover each other.
	Discovery DiscoveryConfig `json:"discovery"`
	// Gossip configures the gossip protocol parameters.
	Gossip GossipConfig `json:"gossip"`
	// Peers is a list of initial peer addresses to join.
	// Used when discovery is disabled or as seed nodes.
	Peers []string `json:"peers,omitempty"`
}

// DiscoveryConfig configures service discovery.
type DiscoveryConfig struct {
	// Enabled enables automatic service discovery.
	Enabled bool `json:"enabled"`
	// Type is the discovery method: kubernetes, consul, etcd, dns, udp, mdns.
	Type string `json:"type"`
	// ServiceName is the service name for discovery.
	ServiceName string `json:"service_name,omitempty"`
	// BindPort is the port for cluster communication.
	// Defaults to 10101.
	BindPort int `json:"bind_port,omitempty"`
	// DialTimeout is the timeout for discovery operations.
	// Defaults to 10s.
	DialTimeout time.Duration `json:"dial_timeout,omitempty"`
	// AllowedAddressSpace is a list of CIDR ranges to filter discovered nodes.
	AllowedAddressSpace []string `json:"allowed_address_space,omitempty"`
	// Kubernetes-specific configuration.
	Kubernetes *KubernetesDiscoveryConfig `json:"kubernetes,omitempty"`
	// Consul-specific configuration.
	Consul *ConsulDiscoveryConfig `json:"consul,omitempty"`
	// Etcd-specific configuration.
	Etcd *EtcdDiscoveryConfig `json:"etcd,omitempty"`
	// DNS-specific configuration.
	DNS *DNSDiscoveryConfig `json:"dns,omitempty"`
	// UDP-specific configuration.
	UDP *UDPDiscoveryConfig `json:"udp,omitempty"`
	// MDNS-specific configuration.
	MDNS *MDNSDiscoveryConfig `json:"mdns,omitempty"`
}

// KubernetesDiscoveryConfig configures Kubernetes-based discovery.
type KubernetesDiscoveryConfig struct {
	// Namespace is the Kubernetes namespace to search for pods.
	// Defaults to "default".
	Namespace string `json:"namespace,omitempty"`
	// LabelSelector is the label selector for pod discovery.
	// Example: "app=koutaku"
	LabelSelector string `json:"label_selector,omitempty"`
}

// ConsulDiscoveryConfig configures Consul-based discovery.
type ConsulDiscoveryConfig struct {
	// Address is the Consul agent address.
	// Defaults to "localhost:8500".
	Address string `json:"address,omitempty"`
	// ServiceName is the service name in Consul.
	ServiceName string `json:"service_name,omitempty"`
	// Datacenter is the Consul datacenter.
	Datacenter string `json:"datacenter,omitempty"`
	// Token is the Consul ACL token.
	Token string `json:"token,omitempty"`
}

// EtcdDiscoveryConfig configures etcd-based discovery.
type EtcdDiscoveryConfig struct {
	// Endpoints is a list of etcd endpoints.
	Endpoints []string `json:"endpoints,omitempty"`
	// ServiceName is the service name in etcd.
	ServiceName string `json:"service_name,omitempty"`
	// Username is the etcd username.
	Username string `json:"username,omitempty"`
	// Password is the etcd password.
	Password string `json:"password,omitempty"`
}

// DNSDiscoveryConfig configures DNS-based discovery.
type DNSDiscoveryConfig struct {
	// Domain is the DNS domain to query for SRV records.
	Domain string `json:"domain,omitempty"`
	// ServiceName is the service name for SRV lookup.
	ServiceName string `json:"service_name,omitempty"`
}

// UDPDiscoveryConfig configures UDP broadcast discovery.
type UDPDiscoveryConfig struct {
	// BindAddress is the address to listen for broadcasts.
	// Defaults to "0.0.0.0:10102".
	BindAddress string `json:"bind_address,omitempty"`
	// BroadcastAddress is the broadcast address to send on.
	// Defaults to "255.255.255.255:10102".
	BroadcastAddress string `json:"broadcast_address,omitempty"`
	// BroadcastInterval is how often to send broadcasts.
	// Defaults to 5s.
	BroadcastInterval time.Duration `json:"broadcast_interval,omitempty"`
}

// MDNSDiscoveryConfig configures mDNS-based discovery.
type MDNSDiscoveryConfig struct {
	// ServiceName is the mDNS service name.
	ServiceName string `json:"service_name,omitempty"`
	// Domain is the mDNS domain.
	// Defaults to "local".
	Domain string `json:"domain,omitempty"`
}

// GossipConfig configures the gossip protocol.
type GossipConfig struct {
	// Port is the gossip protocol port.
	// Defaults to 10101.
	Port int `json:"port,omitempty"`
	// Config contains advanced gossip protocol parameters.
	Config GossipProtocolConfig `json:"config,omitempty"`
}

// GossipProtocolConfig contains advanced gossip protocol parameters.
type GossipProtocolConfig struct {
	// TimeoutSeconds is the health check timeout.
	// Defaults to 10.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// SuccessThreshold is the number of successful checks to mark healthy.
	// Defaults to 3.
	SuccessThreshold int `json:"success_threshold,omitempty"`
	// FailureThreshold is the number of failed checks to mark unhealthy.
	// Defaults to 3.
	FailureThreshold int `json:"failure_threshold,omitempty"`
	// ProbeInterval is the interval between health checks.
	// Defaults to 1s.
	ProbeInterval time.Duration `json:"probe_interval,omitempty"`
	// ProbeTimeout is the timeout for individual probes.
	// Defaults to 500ms.
	ProbeTimeout time.Duration `json:"probe_timeout,omitempty"`
	// GossipInterval is the interval for gossip messages.
	// Defaults to 200ms.
	GossipInterval time.Duration `json:"gossip_interval,omitempty"`
	// GossipNodes is the number of nodes to gossip with per interval.
	// Defaults to 3.
	GossipNodes int `json:"gossip_nodes,omitempty"`
	// RetransmitMult is the multiplier for retransmissions.
	// Defaults to 4.
	RetransmitMult int `json:"retransmit_mult,omitempty"`
	// SuspicionMult is the multiplier for suspicion timeout.
	// Defaults to 4.
	SuspicionMult int `json:"suspicion_mult,omitempty"`
	// PushPullInterval is the interval for full state sync.
	// Defaults to 30s.
	PushPullInterval time.Duration `json:"push_pull_interval,omitempty"`
	// TCPTimeout is the timeout for TCP connections.
	// Defaults to 10s.
	TCPTimeout time.Duration `json:"tcp_timeout,omitempty"`
}

// DefaultClusterConfig returns a ClusterConfig with sensible defaults.
func DefaultClusterConfig() ClusterConfig {
	return ClusterConfig{
		Enabled:     false,
		BindAddress: "0.0.0.0:10101",
		Discovery: DiscoveryConfig{
			Enabled:     false,
			Type:        "kubernetes",
			BindPort:    10101,
			DialTimeout: 10 * time.Second,
			Kubernetes: &KubernetesDiscoveryConfig{
				Namespace:     "default",
				LabelSelector: "app=koutaku",
			},
		},
		Gossip: GossipConfig{
			Port: 10101,
			Config: GossipProtocolConfig{
				TimeoutSeconds:   10,
				SuccessThreshold: 3,
				FailureThreshold: 3,
				ProbeInterval:    1 * time.Second,
				ProbeTimeout:     500 * time.Millisecond,
				GossipInterval:   200 * time.Millisecond,
				GossipNodes:      3,
				RetransmitMult:   4,
				SuspicionMult:    4,
				PushPullInterval: 30 * time.Second,
				TCPTimeout:       10 * time.Second,
			},
		},
	}
}

// ClusterEvent represents an event in the cluster.
type ClusterEvent struct {
	// Type is the event type.
	Type ClusterEventType `json:"type"`
	// Node is the node that triggered the event.
	Node *Node `json:"node"`
	// Timestamp is when the event occurred.
	Timestamp time.Time `json:"timestamp"`
	// Metadata contains additional event data.
	Metadata map[string]string `json:"metadata,omitempty"`
}

// ClusterEventType represents the type of cluster event.
type ClusterEventType int

const (
	// EventNodeJoin is fired when a node joins the cluster.
	EventNodeJoin ClusterEventType = iota
	// EventNodeLeave is fired when a node leaves the cluster.
	EventNodeLeave
	// EventNodeFailed is fired when a node fails.
	EventNodeFailed
	// EventNodeUpdate is fired when a node's metadata changes.
	EventNodeUpdate
	// EventStateSync is fired when cluster state is synchronized.
	EventStateSync
)

// String returns the string representation of ClusterEventType.
func (e ClusterEventType) String() string {
	switch e {
	case EventNodeJoin:
		return "node_join"
	case EventNodeLeave:
		return "node_leave"
	case EventNodeFailed:
		return "node_failed"
	case EventNodeUpdate:
		return "node_update"
	case EventStateSync:
		return "state_sync"
	default:
		return "unknown"
	}
}

// ClusterMetrics contains metrics about the cluster.
type ClusterMetrics struct {
	// TotalNodes is the total number of nodes in the cluster.
	TotalNodes int `json:"total_nodes"`
	// AliveNodes is the number of alive nodes.
	AliveNodes int `json:"alive_nodes"`
	// SuspectNodes is the number of suspect nodes.
	SuspectNodes int `json:"suspect_nodes"`
	// DeadNodes is the number of dead nodes.
	DeadNodes int `json:"dead_nodes"`
	// TotalKeys is the total number of keys in the distributed KV store.
	TotalKeys int `json:"total_keys"`
	// Uptime is the duration this node has been in the cluster.
	Uptime time.Duration `json:"uptime"`
}

// ServiceDiscovery is the interface for service discovery implementations.
type ServiceDiscovery interface {
	// Start begins the discovery process.
	Start(ctx context.Context) error
	// Stop stops the discovery process.
	Stop() error
	// Nodes returns the currently discovered nodes.
	Nodes() ([]*Node, error)
	// Watch returns a channel that receives discovery events.
	Watch() (<-chan DiscoveryEvent, error)
}

// DiscoveryEvent represents a service discovery event.
type DiscoveryEvent struct {
	// Type is the event type.
	Type DiscoveryEventType `json:"type"`
	// Node is the discovered/removed node.
	Node *Node `json:"node"`
	// Timestamp is when the event occurred.
	Timestamp time.Time `json:"timestamp"`
}

// DiscoveryEventType represents the type of discovery event.
type DiscoveryEventType int

const (
	// DiscoveryNodeFound is fired when a new node is discovered.
	DiscoveryNodeFound DiscoveryEventType = iota
	// DiscoveryNodeLost is fired when a node is no longer discoverable.
	DiscoveryNodeLost
)

// String returns the string representation of DiscoveryEventType.
func (e DiscoveryEventType) String() string {
	switch e {
	case DiscoveryNodeFound:
		return "node_found"
	case DiscoveryNodeLost:
		return "node_lost"
	default:
		return "unknown"
	}
}

// GossipMessage represents a message in the gossip protocol.
type GossipMessage struct {
	// Type is the message type.
	Type GossipMessageType `json:"type"`
	// Sender is the node that sent the message.
	Sender *Node `json:"sender"`
	// Nodes contains node state updates.
	Nodes []*Node `json:"nodes,omitempty"`
	// Data contains arbitrary payload data.
	Data []byte `json:"data,omitempty"`
	// Incarnation is the sender's incarnation number.
	Incarnation uint64 `json:"incarnation"`
}

// GossipMessageType represents the type of gossip message.
type GossipMessageType int

const (
	// MsgPing is a health check ping.
	MsgPing GossipMessageType = iota
	// MsgPingReq is an indirect health check request.
	MsgPingReq
	// MsgAck is a health check acknowledgment.
	MsgAck
	// MsgJoin is a join request.
	MsgJoin
	// MsgLeave is a leave notification.
	MsgLeave
	// MsgAlive is a node alive notification.
	MsgAlive
	// MsgSuspect is a node suspect notification.
	MsgSuspect
	// MsgDead is a node dead notification.
	MsgDead
	// MsgPushPull is a full state synchronization.
	MsgPushPull
)

// String returns the string representation of GossipMessageType.
func (m GossipMessageType) String() string {
	switch m {
	case MsgPing:
		return "ping"
	case MsgPingReq:
		return "ping_req"
	case MsgAck:
		return "ack"
	case MsgJoin:
		return "join"
	case MsgLeave:
		return "leave"
	case MsgAlive:
		return "alive"
	case MsgSuspect:
		return "suspect"
	case MsgDead:
		return "dead"
	case MsgPushPull:
		return "push_pull"
	default:
		return "unknown"
	}
}

// Cluster is the main interface for the clustering system.
type Cluster interface {
	// Start starts the cluster node.
	Start(ctx context.Context) error
	// Stop gracefully stops the cluster node.
	Stop() error
	// LocalNode returns the local node information.
	LocalNode() *Node
	// Nodes returns all known nodes in the cluster.
	Nodes() []*Node
	// AliveNodes returns only alive nodes.
	AliveNodes() []*Node
	// Join joins the cluster by connecting to the given peers.
	Join(peers []string) (int, error)
	// Leave gracefully leaves the cluster.
	Leave() error
	// Metrics returns cluster metrics.
	Metrics() ClusterMetrics
	// Events returns a channel that receives cluster events.
	Events() <-chan ClusterEvent
	// IsLeader returns true if this node is the cluster leader.
	IsLeader() bool
	// GetNode returns a specific node by ID.
	GetNode(id string) (*Node, bool)
	// Broadcast sends a message to all nodes.
	Broadcast(msg *GossipMessage) error
	// Send sends a message to a specific node.
	Send(nodeID string, msg *GossipMessage) error
	// NumMembers returns the number of alive members.
	NumMembers() int
}

// Transport is the interface for network transport implementations.
type Transport interface {
	// Listen starts listening for incoming connections.
	Listen(addr string) error
	// Dial connects to a remote node.
	Dial(addr string, timeout time.Duration) (net.Conn, error)
	// Accept accepts incoming connections.
	Accept() (net.Conn, error)
	// Close closes the transport.
	Close() error
	// Addr returns the local address.
	Addr() net.Addr
}
