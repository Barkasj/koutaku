// Package loadbalancer provides adaptive weighted load balancing with
// circuit-breaking for the Koutaku AI Gateway.
package loadbalancer

import "time"

// CircuitState represents the health of a route.
type CircuitState int

const (
	StateHealthy   CircuitState = iota // operating normally
	StateDegraded                      // elevated errors/latency but still usable
	StateFailed                        // circuit open, skip this route
	StateRecovering                    // half-open, probing with limited traffic
)

func (s CircuitState) String() string {
	switch s {
	case StateHealthy:
		return "healthy"
	case StateDegraded:
		return "degraded"
	case StateFailed:
		return "failed"
	case StateRecovering:
		return "recovering"
	default:
		return "unknown"
	}
}

// RouteKey uniquely identifies a provider+key combination.
type RouteKey struct {
	Provider string `json:"provider"`
	KeyID    string `json:"key_id"`
}

func (r RouteKey) String() string {
	if r.KeyID == "" {
		return r.Provider
	}
	return r.Provider + "/" + r.KeyID
}

// RouteMetrics holds rolling-window metrics for a single route.
type RouteMetrics struct {
	Route          RouteKey `json:"route"`
	ErrorRate      float64  `json:"error_rate"`       // 0.0 – 1.0
	LatencyP95Ms   float64  `json:"latency_p95_ms"`   // 95th-percentile latency
	Utilization    float64  `json:"utilization"`       // 0.0 – 1.0 (active / max concurrent)
	Momentum       float64  `json:"momentum"`          // trend indicator: >0 improving, <0 degrading
	TotalRequests  int64    `json:"total_requests"`
	FailedRequests int64    `json:"failed_requests"`
	LastUpdated    time.Time `json:"last_updated"`
}

// RouteState wraps metrics with circuit-breaker state.
type RouteState struct {
	Metrics RouteMetrics  `json:"metrics"`
	Circuit CircuitState  `json:"circuit"`
	// Weight is the computed selection weight (higher = more traffic).
	Weight float64 `json:"weight"`
}

// WeightInputs are the factors fed into the weight calculator.
type WeightInputs struct {
	ErrorRate    float64 // 0.0 – 1.0
	LatencyMs    float64 // p95 latency in ms
	Utilization  float64 // 0.0 – 1.0
	Momentum     float64 // trend: positive = improving
}

// WeightConfig defines the weighting formula coefficients.
type WeightConfig struct {
	ErrorWeight       float64 // default 0.50 — error rate dominates
	LatencyWeight     float64 // default 0.20
	UtilizationWeight float64 // default 0.05
	MomentumWeight    float64 // default 0.25
	BaseWeight        float64 // minimum weight before scaling (default 0.01)
}

// DefaultWeightConfig returns the standard weighting coefficients.
func DefaultWeightConfig() WeightConfig {
	return WeightConfig{
		ErrorWeight:       0.50,
		LatencyWeight:     0.20,
		UtilizationWeight: 0.05,
		MomentumWeight:    0.25,
		BaseWeight:        0.01,
	}
}

// CircuitBreakerConfig tunes the circuit breaker thresholds.
type CircuitBreakerConfig struct {
	// ErrorRateThreshold triggers Degraded state (default 0.30).
	ErrorRateThreshold float64
	// ErrorRateCritical triggers Failed state (default 0.70).
	ErrorRateCritical float64
	// LatencyThresholdMs triggers Degraded state (default 5000).
	LatencyThresholdMs float64
	// RecoveryProbeInterval is how often to probe a Failed route (default 30s).
	RecoveryProbeInterval time.Duration
	// RecoveryProbeCount is successful probes needed to transition to Healthy.
	RecoveryProbeCount int
}

// DefaultCircuitBreakerConfig returns standard thresholds.
func DefaultCircuitBreakerConfig() CircuitBreakerConfig {
	return CircuitBreakerConfig{
		ErrorRateThreshold:    0.30,
		ErrorRateCritical:     0.70,
		LatencyThresholdMs:    5000,
		RecoveryProbeInterval: 30 * time.Second,
		RecoveryProbeCount:    3,
	}
}

// Config is the top-level loadbalancer configuration.
type Config struct {
	WeightConfig        WeightConfig        `json:"weight_config"`
	CircuitBreaker      CircuitBreakerConfig `json:"circuit_breaker"`
	RecomputeIntervalMs int                 `json:"recompute_interval_ms"` // default 5000
	JitterPercent       float64             `json:"jitter_percent"`        // default 0.10
}
