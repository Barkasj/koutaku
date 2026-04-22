package loadbalancer

import (
	"math"
	"sync"
	"time"
)

// MetricsTracker collects and maintains per-route metrics using an
// exponential moving average (EMA) for smooth, memory-efficient tracking.
type MetricsTracker struct {
	mu              sync.RWMutex
	routes          map[string]*routeStateInternal
	decayError      float64 // EMA decay for error rate
	decayLatency    float64 // EMA decay for latency
	decayUtil       float64 // EMA decay for utilization
	windowDuration  time.Duration
}

type routeStateInternal struct {
	metrics       RouteMetrics
	circuit       CircuitState
	consecutiveOK int // used during recovery probing
	lastFailTime  time.Time
}

// NewMetricsTracker creates a tracker with the given EMA decay factors.
// Higher decay = more weight on recent values. Typical: 0.1 – 0.3.
func NewMetricsTracker(decayError, decayLatency, decayUtil float64) *MetricsTracker {
	return &MetricsTracker{
		routes:         make(map[string]*routeStateInternal),
		decayError:     decayError,
		decayLatency:   decayLatency,
		decayUtil:      decayUtil,
		windowDuration: 5 * time.Minute,
	}
}

// RecordRequest records the outcome of a single request.
func (mt *MetricsTracker) RecordRequest(route RouteKey, latencyMs float64, success bool) {
	mt.mu.Lock()
	defer mt.mu.Unlock()

	key := route.String()
	rs, ok := mt.routes[key]
	if !ok {
		rs = &routeStateInternal{
			metrics: RouteMetrics{Route: route},
			circuit: StateHealthy,
		}
		mt.routes[key] = rs
	}

	m := &rs.metrics
	m.TotalRequests++
	if !success {
		m.FailedRequests++
	}
	m.LastUpdated = time.Now().UTC()

	// EMA error rate: 1.0 on failure, 0.0 on success
	errVal := 0.0
	if !success {
		errVal = 1.0
	}
	m.ErrorRate = mt.decayError*errVal + (1-mt.decayError)*m.ErrorRate

	// EMA latency
	m.LatencyP95Ms = mt.decayLatency*latencyMs + (1-mt.decayLatency)*m.LatencyP95Ms

	// Momentum: positive if error rate is improving (decreasing)
	m.Momentum = -(m.ErrorRate - errVal) // simplified trend indicator
}

// RecordUtilization updates the utilization for a route (0.0 – 1.0).
func (mt *MetricsTracker) RecordUtilization(route RouteKey, utilization float64) {
	mt.mu.Lock()
	defer mt.mu.Unlock()

	key := route.String()
	rs, ok := mt.routes[key]
	if !ok {
		rs = &routeStateInternal{
			metrics: RouteMetrics{Route: route},
			circuit: StateHealthy,
		}
		mt.routes[key] = rs
	}
	rs.metrics.Utilization = mt.decayUtil*math.Min(utilization, 1.0) + (1-mt.decayUtil)*rs.metrics.Utilization
}

// GetRouteState returns the current state for a route.
func (mt *MetricsTracker) GetRouteState(route RouteKey) RouteState {
	mt.mu.RLock()
	defer mt.mu.RUnlock()

	key := route.String()
	rs, ok := mt.routes[key]
	if !ok {
		return RouteState{
			Metrics: RouteMetrics{Route: route},
			Circuit: StateHealthy,
			Weight:  1.0,
		}
	}
	return RouteState{
		Metrics: rs.metrics,
		Circuit: rs.circuit,
	}
}

// GetAllRouteStates returns states for all tracked routes.
func (mt *MetricsTracker) GetAllRouteStates() map[string]RouteState {
	mt.mu.RLock()
	defer mt.mu.RUnlock()

	out := make(map[string]RouteState, len(mt.routes))
	for key, rs := range mt.routes {
		out[key] = RouteState{
			Metrics: rs.metrics,
			Circuit: rs.circuit,
		}
	}
	return out
}

// UpdateCircuitStates recalculates circuit breaker state for all routes
// based on current metrics and the provided configuration.
func (mt *MetricsTracker) UpdateCircuitStates(cfg CircuitBreakerConfig) {
	mt.mu.Lock()
	defer mt.mu.Unlock()

	now := time.Now().UTC()
	for _, rs := range mt.routes {
		m := rs.metrics

		switch rs.circuit {
		case StateHealthy:
			if m.ErrorRate >= cfg.ErrorRateCritical {
				rs.circuit = StateFailed
				rs.lastFailTime = now
			} else if m.ErrorRate >= cfg.ErrorRateThreshold || m.LatencyP95Ms >= cfg.LatencyThresholdMs {
				rs.circuit = StateDegraded
			}

		case StateDegraded:
			if m.ErrorRate >= cfg.ErrorRateCritical {
				rs.circuit = StateFailed
				rs.lastFailTime = now
			} else if m.ErrorRate < cfg.ErrorRateThreshold && m.LatencyP95Ms < cfg.LatencyThresholdMs {
				rs.circuit = StateHealthy
			}

		case StateFailed:
			// Move to Recovering after the probe interval
			if now.Sub(rs.lastFailTime) >= cfg.RecoveryProbeInterval {
				rs.circuit = StateRecovering
				rs.consecutiveOK = 0
			}

		case StateRecovering:
			if m.ErrorRate >= cfg.ErrorRateCritical {
				rs.circuit = StateFailed
				rs.lastFailTime = now
				rs.consecutiveOK = 0
			} else if m.ErrorRate < cfg.ErrorRateThreshold {
				rs.consecutiveOK++
				if rs.consecutiveOK >= cfg.RecoveryProbeCount {
					rs.circuit = StateHealthy
					rs.consecutiveOK = 0
				}
			} else {
				rs.consecutiveOK = 0
			}
		}
	}
}

// DecayMetrics applies time-based decay to all route metrics so stale
// data doesn't dominate. Call periodically (e.g., every recompute cycle).
func (mt *MetricsTracker) DecayMetrics(factor float64) {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	for _, rs := range mt.routes {
		rs.metrics.ErrorRate *= factor
		rs.metrics.Utilization *= factor
		rs.metrics.Momentum *= factor
	}
}
