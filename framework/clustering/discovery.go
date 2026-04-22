package clustering

import (
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

// KubernetesDiscovery implements service discovery for Kubernetes environments.
type KubernetesDiscovery struct {
	config   DiscoveryConfig
	nodes    map[string]*Node
	mu       sync.RWMutex
	eventCh  chan DiscoveryEvent
	stopCh   chan struct{}
	logger   *log.Logger
}

// NewKubernetesDiscovery creates a new Kubernetes-based service discovery.
func NewKubernetesDiscovery(config DiscoveryConfig) *KubernetesDiscovery {
	return &KubernetesDiscovery{
		config:  config,
		nodes:   make(map[string]*Node),
		eventCh: make(chan DiscoveryEvent, 100),
		stopCh:  make(chan struct{}),
		logger:  log.Default(),
	}
}

// Start begins the Kubernetes discovery process.
func (kd *KubernetesDiscovery) Start(ctx context.Context) error {
	kd.logger.Printf("[Discovery:K8s] Starting Kubernetes discovery (namespace=%s, selector=%s)",
		kd.config.Kubernetes.Namespace, kd.config.Kubernetes.LabelSelector)

	// In a real implementation, this would use the Kubernetes API.
	// For now, we'll simulate discovery.
	go kd.watchLoop(ctx)

	return nil
}

// Stop stops the Kubernetes discovery process.
func (kd *KubernetesDiscovery) Stop() error {
	close(kd.stopCh)
	return nil
}

// Nodes returns the currently discovered nodes.
func (kd *KubernetesDiscovery) Nodes() ([]*Node, error) {
	kd.mu.RLock()
	defer kd.mu.RUnlock()

	nodes := make([]*Node, 0, len(kd.nodes))
	for _, node := range kd.nodes {
		nodes = append(nodes, node)
	}
	return nodes, nil
}

// Watch returns a channel that receives discovery events.
func (kd *KubernetesDiscovery) Watch() (<-chan DiscoveryEvent, error) {
	return kd.eventCh, nil
}

// watchLoop watches for Kubernetes pod changes.
func (kd *KubernetesDiscovery) watchLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-kd.stopCh:
			return
		case <-ticker.C:
			// In a real implementation, this would query the Kubernetes API
			// for pods matching the label selector.
			// For now, we'll just log that we're watching.
			kd.logger.Printf("[Discovery:K8s] Watching for pods...")
		}
	}
}

// ConsulDiscovery implements service discovery for Consul environments.
type ConsulDiscovery struct {
	config   DiscoveryConfig
	nodes    map[string]*Node
	mu       sync.RWMutex
	eventCh  chan DiscoveryEvent
	stopCh   chan struct{}
	logger   *log.Logger
}

// NewConsulDiscovery creates a new Consul-based service discovery.
func NewConsulDiscovery(config DiscoveryConfig) *ConsulDiscovery {
	return &ConsulDiscovery{
		config:  config,
		nodes:   make(map[string]*Node),
		eventCh: make(chan DiscoveryEvent, 100),
		stopCh:  make(chan struct{}),
		logger:  log.Default(),
	}
}

// Start begins the Consul discovery process.
func (cd *ConsulDiscovery) Start(ctx context.Context) error {
	cd.logger.Printf("[Discovery:Consul] Starting Consul discovery (address=%s, service=%s)",
		cd.config.Consul.Address, cd.config.Consul.ServiceName)

	go cd.watchLoop(ctx)

	return nil
}

// Stop stops the Consul discovery process.
func (cd *ConsulDiscovery) Stop() error {
	close(cd.stopCh)
	return nil
}

// Nodes returns the currently discovered nodes.
func (cd *ConsulDiscovery) Nodes() ([]*Node, error) {
	cd.mu.RLock()
	defer cd.mu.RUnlock()

	nodes := make([]*Node, 0, len(cd.nodes))
	for _, node := range cd.nodes {
		nodes = append(nodes, node)
	}
	return nodes, nil
}

// Watch returns a channel that receives discovery events.
func (cd *ConsulDiscovery) Watch() (<-chan DiscoveryEvent, error) {
	return cd.eventCh, nil
}

// watchLoop watches for Consul service changes.
func (cd *ConsulDiscovery) watchLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-cd.stopCh:
			return
		case <-ticker.C:
			cd.logger.Printf("[Discovery:Consul] Watching for services...")
		}
	}
}

// EtcdDiscovery implements service discovery for etcd environments.
type EtcdDiscovery struct {
	config   DiscoveryConfig
	nodes    map[string]*Node
	mu       sync.RWMutex
	eventCh  chan DiscoveryEvent
	stopCh   chan struct{}
	logger   *log.Logger
}

// NewEtcdDiscovery creates a new etcd-based service discovery.
func NewEtcdDiscovery(config DiscoveryConfig) *EtcdDiscovery {
	return &EtcdDiscovery{
		config:  config,
		nodes:   make(map[string]*Node),
		eventCh: make(chan DiscoveryEvent, 100),
		stopCh:  make(chan struct{}),
		logger:  log.Default(),
	}
}

// Start begins the etcd discovery process.
func (ed *EtcdDiscovery) Start(ctx context.Context) error {
	ed.logger.Printf("[Discovery:etcd] Starting etcd discovery (endpoints=%v, service=%s)",
		ed.config.Etcd.Endpoints, ed.config.Etcd.ServiceName)

	go ed.watchLoop(ctx)

	return nil
}

// Stop stops the etcd discovery process.
func (ed *EtcdDiscovery) Stop() error {
	close(ed.stopCh)
	return nil
}

// Nodes returns the currently discovered nodes.
func (ed *EtcdDiscovery) Nodes() ([]*Node, error) {
	ed.mu.RLock()
	defer ed.mu.RUnlock()

	nodes := make([]*Node, 0, len(ed.nodes))
	for _, node := range ed.nodes {
		nodes = append(nodes, node)
	}
	return nodes, nil
}

// Watch returns a channel that receives discovery events.
func (ed *EtcdDiscovery) Watch() (<-chan DiscoveryEvent, error) {
	return ed.eventCh, nil
}

// watchLoop watches for etcd service changes.
func (ed *EtcdDiscovery) watchLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ed.stopCh:
			return
		case <-ticker.C:
			ed.logger.Printf("[Discovery:etcd] Watching for services...")
		}
	}
}

// DNSDiscovery implements service discovery using DNS SRV records.
type DNSDiscovery struct {
	config   DiscoveryConfig
	nodes    map[string]*Node
	mu       sync.RWMutex
	eventCh  chan DiscoveryEvent
	stopCh   chan struct{}
	logger   *log.Logger
}

// NewDNSDiscovery creates a new DNS-based service discovery.
func NewDNSDiscovery(config DiscoveryConfig) *DNSDiscovery {
	return &DNSDiscovery{
		config:  config,
		nodes:   make(map[string]*Node),
		eventCh: make(chan DiscoveryEvent, 100),
		stopCh:  make(chan struct{}),
		logger:  log.Default(),
	}
}

// Start begins the DNS discovery process.
func (dd *DNSDiscovery) Start(ctx context.Context) error {
	dd.logger.Printf("[Discovery:DNS] Starting DNS discovery (domain=%s, service=%s)",
		dd.config.DNS.Domain, dd.config.DNS.ServiceName)

	go dd.watchLoop(ctx)

	return nil
}

// Stop stops the DNS discovery process.
func (dd *DNSDiscovery) Stop() error {
	close(dd.stopCh)
	return nil
}

// Nodes returns the currently discovered nodes.
func (dd *DNSDiscovery) Nodes() ([]*Node, error) {
	dd.mu.RLock()
	defer dd.mu.RUnlock()

	nodes := make([]*Node, 0, len(dd.nodes))
	for _, node := range dd.nodes {
		nodes = append(nodes, node)
	}
	return nodes, nil
}

// Watch returns a channel that receives discovery events.
func (dd *DNSDiscovery) Watch() (<-chan DiscoveryEvent, error) {
	return dd.eventCh, nil
}

// watchLoop watches for DNS SRV record changes.
func (dd *DNSDiscovery) watchLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-dd.stopCh:
			return
		case <-ticker.C:
			dd.querySRV()
		}
	}
}

// querySRV queries DNS SRV records for nodes.
func (dd *DNSDiscovery) querySRV() {
	serviceName := dd.config.DNS.ServiceName
	if serviceName == "" {
		serviceName = "koutaku-gossip"
	}

	domain := dd.config.DNS.Domain
	if domain == "" {
		domain = "cluster.local"
	}

	// Query SRV records.
	_, addrs, err := net.LookupSRV(serviceName, "tcp", domain)
	if err != nil {
		dd.logger.Printf("[Discovery:DNS] SRV lookup failed: %v", err)
		return
	}

	dd.mu.Lock()
	defer dd.mu.Unlock()

	// Track current nodes.
	currentNodes := make(map[string]bool)

	for _, addr := range addrs {
		nodeAddr := fmt.Sprintf("%s:%d", strings.TrimSuffix(addr.Target, "."), addr.Port)
		currentNodes[nodeAddr] = true

		if _, exists := dd.nodes[nodeAddr]; !exists {
			node := &Node{
				ID:       nodeAddr,
				Name:     addr.Target,
				Address:  nodeAddr,
				State:    NodeStateAlive,
				LastSeen: time.Now(),
				Metadata: make(map[string]string),
			}

			dd.nodes[nodeAddr] = node
			dd.eventCh <- DiscoveryEvent{
				Type:      DiscoveryNodeFound,
				Node:      node,
				Timestamp: time.Now(),
			}

			dd.logger.Printf("[Discovery:DNS] Found node: %s", nodeAddr)
		}
	}

	// Remove nodes that are no longer in DNS.
	for addr, node := range dd.nodes {
		if !currentNodes[addr] {
			delete(dd.nodes, addr)
			dd.eventCh <- DiscoveryEvent{
				Type:      DiscoveryNodeLost,
				Node:      node,
				Timestamp: time.Now(),
			}
			dd.logger.Printf("[Discovery:DNS] Lost node: %s", addr)
		}
	}
}

// UDPDiscovery implements service discovery using UDP broadcasts.
type UDPDiscovery struct {
	config   DiscoveryConfig
	nodes    map[string]*Node
	mu       sync.RWMutex
	eventCh  chan DiscoveryEvent
	stopCh   chan struct{}
	logger   *log.Logger
	localAddr string
}

// NewUDPDiscovery creates a new UDP broadcast-based service discovery.
func NewUDPDiscovery(config DiscoveryConfig) *UDPDiscovery {
	return &UDPDiscovery{
		config:  config,
		nodes:   make(map[string]*Node),
		eventCh: make(chan DiscoveryEvent, 100),
		stopCh:  make(chan struct{}),
		logger:  log.Default(),
	}
}

// Start begins the UDP discovery process.
func (ud *UDPDiscovery) Start(ctx context.Context) error {
	bindAddr := ud.config.UDP.BindAddress
	if bindAddr == "" {
		bindAddr = "0.0.0.0:10102"
	}

	ud.logger.Printf("[Discovery:UDP] Starting UDP discovery (bind=%s)", bindAddr)

	// Parse bind address.
	addr, err := net.ResolveUDPAddr("udp", bindAddr)
	if err != nil {
		return fmt.Errorf("failed to resolve UDP address: %w", err)
	}

	// Create listener.
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on UDP: %w", err)
	}

	ud.localAddr = bindAddr

	// Start receiver.
	go ud.receiveLoop(ctx, conn)

	// Start broadcaster.
	go ud.broadcastLoop(ctx)

	return nil
}

// Stop stops the UDP discovery process.
func (ud *UDPDiscovery) Stop() error {
	close(ud.stopCh)
	return nil
}

// Nodes returns the currently discovered nodes.
func (ud *UDPDiscovery) Nodes() ([]*Node, error) {
	ud.mu.RLock()
	defer ud.mu.RUnlock()

	nodes := make([]*Node, 0, len(ud.nodes))
	for _, node := range ud.nodes {
		nodes = append(nodes, node)
	}
	return nodes, nil
}

// Watch returns a channel that receives discovery events.
func (ud *UDPDiscovery) Watch() (<-chan DiscoveryEvent, error) {
	return ud.eventCh, nil
}

// receiveLoop receives UDP broadcast messages.
func (ud *UDPDiscovery) receiveLoop(ctx context.Context, conn *net.UDPConn) {
	buf := make([]byte, 1024)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ud.stopCh:
			return
		default:
		}

		conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			ud.logger.Printf("[Discovery:UDP] Read error: %v", err)
			continue
		}

		// Parse message.
		msg := string(buf[:n])
		if strings.HasPrefix(msg, "KOUTAKU_DISCOVERY:") {
			parts := strings.Split(msg, ":")
			if len(parts) >= 3 {
				nodeID := parts[1]
				nodeAddr := parts[2]

				// Ignore our own broadcasts.
				if nodeAddr == ud.localAddr {
					continue
				}

				ud.mu.Lock()
				if _, exists := ud.nodes[nodeAddr]; !exists {
					node := &Node{
						ID:       nodeID,
						Name:     fmt.Sprintf("node-%s", nodeID[:8]),
						Address:  nodeAddr,
						State:    NodeStateAlive,
						LastSeen: time.Now(),
						Metadata: make(map[string]string),
					}

					ud.nodes[nodeAddr] = node
					ud.eventCh <- DiscoveryEvent{
						Type:      DiscoveryNodeFound,
						Node:      node,
						Timestamp: time.Now(),
					}

					ud.logger.Printf("[Discovery:UDP] Found node: %s (%s)", nodeID[:8], nodeAddr)
				} else {
					ud.nodes[nodeAddr].LastSeen = time.Now()
				}
				ud.mu.Unlock()
			}
		}
	}
}

// broadcastLoop sends periodic UDP broadcasts.
func (ud *UDPDiscovery) broadcastLoop(ctx context.Context) {
	broadcastInterval := ud.config.UDP.BroadcastInterval
	if broadcastInterval <= 0 {
		broadcastInterval = 5 * time.Second
	}

	ticker := time.NewTicker(broadcastInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ud.stopCh:
			return
		case <-ticker.C:
			ud.sendBroadcast()
		}
	}
}

// sendBroadcast sends a UDP broadcast message.
func (ud *UDPDiscovery) sendBroadcast() {
	broadcastAddr := ud.config.UDP.BroadcastAddress
	if broadcastAddr == "" {
		broadcastAddr = "255.255.255.255:10102"
	}

	addr, err := net.ResolveUDPAddr("udp", broadcastAddr)
	if err != nil {
		ud.logger.Printf("[Discovery:UDP] Failed to resolve broadcast address: %v", err)
		return
	}

	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		ud.logger.Printf("[Discovery:UDP] Failed to dial broadcast: %v", err)
		return
	}
	defer conn.Close()

	// Enable broadcast.
	if err := conn.SetWriteBuffer(65536); err != nil {
		ud.logger.Printf("[Discovery:UDP] Failed to set write buffer: %v", err)
	}

	nodeID := ud.localAddr // Use local address as node ID for simplicity.
	msg := fmt.Sprintf("KOUTAKU_DISCOVERY:%s:%s", nodeID, ud.localAddr)

	_, err = conn.Write([]byte(msg))
	if err != nil {
		ud.logger.Printf("[Discovery:UDP] Failed to send broadcast: %v", err)
	}
}

// MDNSDiscovery implements service discovery using mDNS (multicast DNS).
type MDNSDiscovery struct {
	config   DiscoveryConfig
	nodes    map[string]*Node
	mu       sync.RWMutex
	eventCh  chan DiscoveryEvent
	stopCh   chan struct{}
	logger   *log.Logger
}

// NewMDNSDiscovery creates a new mDNS-based service discovery.
func NewMDNSDiscovery(config DiscoveryConfig) *MDNSDiscovery {
	return &MDNSDiscovery{
		config:  config,
		nodes:   make(map[string]*Node),
		eventCh: make(chan DiscoveryEvent, 100),
		stopCh:  make(chan struct{}),
		logger:  log.Default(),
	}
}

// Start begins the mDNS discovery process.
func (md *MDNSDiscovery) Start(ctx context.Context) error {
	serviceName := md.config.MDNS.ServiceName
	if serviceName == "" {
		serviceName = "_koutaku-gossip._tcp"
	}

	domain := md.config.MDNS.Domain
	if domain == "" {
		domain = "local"
	}

	md.logger.Printf("[Discovery:mDNS] Starting mDNS discovery (service=%s, domain=%s)", serviceName, domain)

	go md.watchLoop(ctx)

	return nil
}

// Stop stops the mDNS discovery process.
func (md *MDNSDiscovery) Stop() error {
	close(md.stopCh)
	return nil
}

// Nodes returns the currently discovered nodes.
func (md *MDNSDiscovery) Nodes() ([]*Node, error) {
	md.mu.RLock()
	defer md.mu.RUnlock()

	nodes := make([]*Node, 0, len(md.nodes))
	for _, node := range md.nodes {
		nodes = append(nodes, node)
	}
	return nodes, nil
}

// Watch returns a channel that receives discovery events.
func (md *MDNSDiscovery) Watch() (<-chan DiscoveryEvent, error) {
	return md.eventCh, nil
}

// watchLoop watches for mDNS service announcements.
func (md *MDNSDiscovery) watchLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-md.stopCh:
			return
		case <-ticker.C:
			md.logger.Printf("[Discovery:mDNS] Watching for services...")
		}
	}
}
