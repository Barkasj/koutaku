package loadbalancer

import (
	"math"
	"math/rand"
	"sort"
	"sync"
	"time"
)

// WeightEngine computes and caches route weights, performing async
// recomputation on a configurable interval.
type WeightEngine struct {
	mu             sync.RWMutex
	tracker        *MetricsTracker
	config         WeightConfig
	cbConfig       CircuitBreakerConfig
	jitterPct      float64
	weights        map[string]float64 // routeKey -> computed weight
	cancel         chan struct{}
	wg             sync.WaitGroup
}

// NewWeightEngine creates a WeightEngine. Call Start() to begin async
// recomputation; Stop() to shut down.
func NewWeightEngine(tracker *MetricsTracker, cfg Config) *WeightEngine {
	jitter := cfg.JitterPercent
	if jitter <= 0 {
		jitter = 0.10
	}
	return &WeightEngine{
		tracker:   tracker,
		config:    cfg.WeightConfig,
		cbConfig:  cfg.CircuitBreaker,
		jitterPct: jitter,
		weights:   make(map[string]float64),
		cancel:    make(chan struct{}),
	}
}

// Start launches the background recomputation goroutine.
func (we *WeightEngine) Start(intervalMs int) {
	if intervalMs <= 0 {
		intervalMs = 5000
	}
	we.wg.Add(1)
	go func() {
		defer we.wg.Done()
		ticker := time.NewTicker(time.Duration(intervalMs) * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-we.cancel:
				return
			case <-ticker.C:
				we.recompute()
			}
		}
	}()
}

// Stop signals the background goroutine to exit.
func (we *WeightEngine) Stop() {
	close(we.cancel)
	we.wg.Wait()
}

// SelectRoute performs weighted random selection among healthy/degraded
// routes. Returns the selected route key and its state.
// If all routes are failed, returns the one with the lowest error rate.
func (we *WeightEngine) SelectRoute(routes []RouteKey) (RouteKey, RouteState, error) {
	we.mu.RLock()
	defer we.mu.RUnlock()

	type candidate struct {
		route  RouteKey
		state  RouteState
		weight float64
	}

	var candidates []candidate
	for _, r := range routes {
		state := we.tracker.GetRouteState(r)
		if state.Circuit == StateFailed {
			continue
		}
		w := we.computeWeight(state)
		// Apply jitter
		jitter := 1.0 + (rand.Float64()*2-1)*we.jitterPct
		w *= jitter
		if w < we.config.BaseWeight {
			w = we.config.BaseWeight
		}
		candidates = append(candidates, candidate{route: r, state: state, weight: w})
	}

	if len(candidates) == 0 {
		// All failed — fallback to least-bad route
		var leastBad *candidate
		for _, r := range routes {
			state := we.tracker.GetRouteState(r)
			c := candidate{route: r, state: state, weight: we.config.BaseWeight}
			if leastBad == nil || state.Metrics.ErrorRate < leastBad.state.Metrics.ErrorRate {
				leastBad = &c
			}
		}
		if leastBad != nil {
			return leastBad.route, leastBad.state, nil
		}
		return routes[0], RouteState{}, nil
	}

	// Weighted random selection
	totalWeight := 0.0
	for _, c := range candidates {
		totalWeight += c.weight
	}

	roll := rand.Float64() * totalWeight
	cumulative := 0.0
	for _, c := range candidates {
		cumulative += c.weight
		if roll <= cumulative {
			return c.route, c.state, nil
		}
	}

	// Fallback
	return candidates[0].route, candidates[0].state, nil
}

// SelectProviderKey performs two-level selection: first selects a provider,
// then selects a key within that provider.
func (we *WeightEngine) SelectProviderKey(routes []RouteKey) (RouteKey, RouteState, error) {
	// Group by provider
	providers := make(map[string][]RouteKey)
	for _, r := range routes {
		providers[r.Provider] = append(providers[r.Provider], r)
	}

	// Build provider-level routes (aggregate weight)
	type provCandidate struct {
		provider string
		keys     []RouteKey
		weight   float64
		state    RouteState
	}

	var provCandidates []provCandidate
	for prov, keys := range providers {
		// Aggregate weight across all keys in this provider
		var totalWeight float64
		var bestState RouteState
		for _, k := range keys {
			state := we.tracker.GetRouteState(k)
			if state.Circuit == StateFailed {
				continue
			}
			w := we.computeWeight(state)
			jitter := 1.0 + (rand.Float64()*2-1)*we.jitterPct
			totalWeight += w * jitter
			if bestState.Circuit == 0 || state.Metrics.ErrorRate < bestState.Metrics.ErrorRate {
				bestState = state
			}
		}
		if totalWeight > 0 {
			provCandidates = append(provCandidates, provCandidate{
				provider: prov,
				keys:     keys,
				weight:   totalWeight,
				state:    bestState,
			})
		}
	}

	if len(provCandidates) == 0 {
		return routes[0], RouteState{}, nil
	}

	// Sort by weight descending for deterministic fallback
	sort.Slice(provCandidates, func(i, j int) bool {
		return provCandidates[i].weight > provCandidates[j].weight
	})

	// Select provider
	totalWeight := 0.0
	for _, c := range provCandidates {
		totalWeight += c.weight
	}
	roll := rand.Float64() * totalWeight
	cumulative := 0.0
	var selected provCandidate
	for _, c := range provCandidates {
		cumulative += c.weight
		if roll <= cumulative {
			selected = c
			break
		}
	}
	if selected.provider == "" {
		selected = provCandidates[0]
	}

	// Now select a key within the chosen provider
	return we.SelectRoute(selected.keys)
}

// GetWeights returns a snapshot of all computed weights.
func (we *WeightEngine) GetWeights() map[string]float64 {
	we.mu.RLock()
	defer we.mu.RUnlock()
	out := make(map[string]float64, len(we.weights))
	for k, v := range we.weights {
		out[k] = v
	}
	return out
}

// --- internal ---

func (we *WeightEngine) recompute() {
	// Update circuit breaker states first
	we.tracker.UpdateCircuitStates(we.cbConfig)

	states := we.tracker.GetAllRouteStates()
	newWeights := make(map[string]float64, len(states))

	for key, state := range states {
		if state.Circuit == StateFailed {
			newWeights[key] = 0
			continue
		}
		w := we.computeWeight(state)
		if w < we.config.BaseWeight {
			w = we.config.BaseWeight
		}
		newWeights[key] = w
	}

	we.mu.Lock()
	we.weights = newWeights
	we.mu.Unlock()
}

func (we *WeightEngine) computeWeight(state RouteState) float64 {
	m := state.Metrics

	// Error score: 1.0 when error rate is 0, 0.0 when error rate is 1.0
	errorScore := math.Max(0, 1.0-m.ErrorRate)

	// Latency score: inverse of latency, normalised. Assume 10000ms is the worst.
	latencyScore := math.Max(0, 1.0-m.LatencyP95Ms/10000.0)

	// Utilisation score: lower is better
	utilScore := math.Max(0, 1.0-m.Utilization)

	// Momentum: clamp to [-1, 1], shift to [0, 1]
	momentumScore := math.Max(0, math.Min(1, 0.5+m.Momentum))

	w := we.config.ErrorWeight*errorScore +
		we.config.LatencyWeight*latencyScore +
		we.config.UtilizationWeight*utilScore +
		we.config.MomentumWeight*momentumScore

	// Recovering routes get halved weight
	if state.Circuit == StateRecovering {
		w *= 0.5
	}
	// Degraded routes get 75% weight
	if state.Circuit == StateDegraded {
		w *= 0.75
	}

	return math.Max(we.config.BaseWeight, w)
}
