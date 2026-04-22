package loadbalancer

import (
	"context"
	"sync"
)

const PluginName = "loadbalancer"

// Plugin is the adaptive load balancer plugin for Koutaku.
type Plugin struct {
	mu      sync.RWMutex
	tracker *MetricsTracker
	engine  *WeightEngine
	config  Config
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// New creates a loadbalancer plugin with the given configuration.
func New(cfg Config) *Plugin {
	weightCfg := cfg.WeightConfig
	if weightCfg.ErrorWeight == 0 && weightCfg.LatencyWeight == 0 {
		weightCfg = DefaultWeightConfig()
	}
	cbCfg := cfg.CircuitBreaker
	if cbCfg.ErrorRateThreshold == 0 {
		cbCfg = DefaultCircuitBreakerConfig()
	}

	tracker := NewMetricsTracker(0.15, 0.10, 0.10)
	engine := NewWeightEngine(tracker, Config{
		WeightConfig:   weightCfg,
		CircuitBreaker: cbCfg,
		JitterPercent:  cfg.JitterPercent,
	})

	return &Plugin{
		tracker: tracker,
		engine:  engine,
		config:  cfg,
	}
}

// GetName returns the plugin name.
func (p *Plugin) GetName() string { return PluginName }

// Start launches the background weight recomputation loop.
func (p *Plugin) Start(ctx context.Context) {
	ctx, p.cancel = context.WithCancel(ctx)
	interval := p.config.RecomputeIntervalMs
	if interval <= 0 {
		interval = 5000
	}
	p.engine.Start(interval)

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		<-ctx.Done()
		p.engine.Stop()
	}()
}

// Cleanup stops background workers.
func (p *Plugin) Cleanup() error {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
	return nil
}

// --- Public API ---

// RecordRequest records the outcome of a request routed to a specific provider+key.
func (p *Plugin) RecordRequest(provider, keyID string, latencyMs float64, success bool) {
	p.tracker.RecordRequest(RouteKey{Provider: provider, KeyID: keyID}, latencyMs, success)
}

// RecordUtilization updates the utilization for a provider+key.
func (p *Plugin) RecordUtilization(provider, keyID string, utilization float64) {
	p.tracker.RecordUtilization(RouteKey{Provider: provider, KeyID: keyID}, utilization)
}

// SelectRoute picks the best route from the given provider+key combinations.
func (p *Plugin) SelectRoute(routes []RouteKey) (RouteKey, RouteState) {
	key, state, _ := p.engine.SelectRoute(routes)
	return key, state
}

// SelectProviderKey performs two-level selection: provider then key.
func (p *Plugin) SelectProviderKey(routes []RouteKey) (RouteKey, RouteState) {
	key, state, _ := p.engine.SelectProviderKey(routes)
	return key, state
}

// GetRouteState returns the current state for a specific route.
func (p *Plugin) GetRouteState(provider, keyID string) RouteState {
	return p.tracker.GetRouteState(RouteKey{Provider: provider, KeyID: keyID})
}

// GetAllRouteStates returns states for all tracked routes.
func (p *Plugin) GetAllRouteStates() map[string]RouteState {
	return p.tracker.GetAllRouteStates()
}

// GetWeights returns the current computed weights for all routes.
func (p *Plugin) GetWeights() map[string]float64 {
	return p.engine.GetWeights()
}
