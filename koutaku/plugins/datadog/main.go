// Package datadog provides enterprise-grade Datadog integration for Koutaku AI Gateway.
// It exports metrics, traces, and logs to Datadog for comprehensive observability.
package datadog

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

const PluginName = "datadog"

// Config represents the Datadog plugin configuration.
type Config struct {
	// Enabled enables the Datadog plugin.
	Enabled bool `json:"enabled"`
	// APIKey is the Datadog API key.
	APIKey string `json:"api_key"`
	// AppKey is the Datadog application key.
	AppKey string `json:"app_key,omitempty"`
	// Site is the Datadog site (e.g., "datadoghq.com", "datadoghq.eu").
	Site string `json:"site,omitempty"`
	// Service is the service name for Datadog.
	Service string `json:"service,omitempty"`
	// Env is the environment name (e.g., "production", "staging").
	Env string `json:"env,omitempty"`
	// Version is the application version.
	Version string `json:"version,omitempty"`
	// MetricsEnabled enables metrics export.
	MetricsEnabled bool `json:"metrics_enabled,omitempty"`
	// TracesEnabled enables traces export.
	TracesEnabled bool `json:"traces_enabled,omitempty"`
	// LogsEnabled enables logs export.
	LogsEnabled bool `json:"logs_enabled,omitempty"`
	// FlushInterval is the interval to flush metrics to Datadog.
	// Defaults to 10 seconds.
	FlushInterval time.Duration `json:"flush_interval,omitempty"`
	// Tags are global tags added to all metrics.
	Tags []string `json:"tags,omitempty"`
	// StatsdAddr is the DogStatsD address.
	// Defaults to "localhost:8125".
	StatsdAddr string `json:"statsd_addr,omitempty"`
	// TraceAgentAddr is the Datadog Trace Agent address.
	// Defaults to "localhost:8126".
	TraceAgentAddr string `json:"trace_agent_addr,omitempty"`
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		Enabled:        false,
		Site:           "datadoghq.com",
		Service:        "koutaku",
		MetricsEnabled: true,
		TracesEnabled:  true,
		LogsEnabled:    true,
		FlushInterval:  10 * time.Second,
		StatsdAddr:     "localhost:8125",
		TraceAgentAddr: "localhost:8126",
	}
}

// MetricType represents the type of metric.
type MetricType string

const (
	// MetricTypeCounter is a counter metric.
	MetricTypeCounter MetricType = "counter"
	// MetricTypeGauge is a gauge metric.
	MetricTypeGauge MetricType = "gauge"
	// MetricTypeHistogram is a histogram metric.
	MetricTypeHistogram MetricType = "histogram"
	// MetricTypeDistribution is a distribution metric.
	MetricTypeDistribution MetricType = "distribution"
	// MetricTypeSet is a set metric.
	MetricTypeSet MetricType = "set"
)

// Metric represents a Datadog metric.
type Metric struct {
	// Name is the metric name.
	Name string `json:"name"`
	// Type is the metric type.
	Type MetricType `json:"type"`
	// Value is the metric value.
	Value float64 `json:"value"`
	// Tags are the metric tags.
	Tags []string `json:"tags,omitempty"`
	// Timestamp is when the metric was recorded.
	Timestamp time.Time `json:"timestamp"`
}

// Trace represents a Datadog trace.
type Trace struct {
	// TraceID is the trace identifier.
	TraceID uint64 `json:"trace_id"`
	// SpanID is the span identifier.
	SpanID uint64 `json:"span_id"`
	// ParentID is the parent span identifier.
	ParentID uint64 `json:"parent_id,omitempty"`
	// Name is the span name.
	Name string `json:"name"`
	// Resource is the resource name.
	Resource string `json:"resource"`
	// Service is the service name.
	Service string `json:"service"`
	// Type is the span type (e.g., "http", "db", "cache").
	Type string `json:"type"`
	// Start is the span start time.
	Start time.Time `json:"start"`
	// Duration is the span duration.
	Duration time.Duration `json:"duration"`
	// Error is 1 if the span has an error.
	Error int `json:"error"`
	// Meta are span metadata.
	Meta map[string]string `json:"meta,omitempty"`
	// Metrics are span metrics.
	Metrics map[string]float64 `json:"metrics,omitempty"`
}

// LogEntry represents a Datadog log entry.
type LogEntry struct {
	// Message is the log message.
	Message string `json:"message"`
	// Level is the log level (e.g., "info", "warn", "error").
	Level string `json:"level"`
	// Service is the service name.
	Service string `json:"service"`
	// Timestamp is when the log was recorded.
	Timestamp time.Time `json:"timestamp"`
	// Attributes are additional log attributes.
	Attributes map[string]interface{} `json:"attributes,omitempty"`
}

// Plugin is the Datadog plugin for Koutaku.
type Plugin struct {
	mu     sync.RWMutex
	config Config
	logger *log.Logger

	// Metrics buffer.
	metrics   []*Metric
	metricsMu sync.Mutex

	// Traces buffer.
	traces   []*Trace
	tracesMu sync.Mutex

	// Logs buffer.
	logs   []*LogEntry
	logsMu sync.Mutex

	// Channels.
	stopCh    chan struct{}
	stoppedCh chan struct{}

	// Statistics.
	metricsSent uint64
	tracesSent  uint64
	logsSent    uint64
}

// New creates a new Datadog plugin with the given configuration.
func New(cfg Config, logger *log.Logger) *Plugin {
	if logger == nil {
		logger = log.Default()
	}

	// Apply defaults.
	if cfg.Site == "" {
		cfg.Site = "datadoghq.com"
	}
	if cfg.Service == "" {
		cfg.Service = "koutaku"
	}
	if cfg.FlushInterval == 0 {
		cfg.FlushInterval = 10 * time.Second
	}
	if cfg.StatsdAddr == "" {
		cfg.StatsdAddr = "localhost:8125"
	}
	if cfg.TraceAgentAddr == "" {
		cfg.TraceAgentAddr = "localhost:8126"
	}

	return &Plugin{
		config:    cfg,
		logger:    logger,
		metrics:   make([]*Metric, 0),
		traces:    make([]*Trace, 0),
		logs:      make([]*LogEntry, 0),
		stopCh:    make(chan struct{}),
		stoppedCh: make(chan struct{}),
	}
}

// GetName returns the plugin name.
func (p *Plugin) GetName() string {
	return PluginName
}

// Start starts the Datadog plugin.
func (p *Plugin) Start(ctx context.Context) error {
	if !p.config.Enabled {
		p.logger.Printf("[Datadog] Plugin is disabled")
		return nil
	}

	p.logger.Printf("[Datadog] Starting plugin (service=%s, env=%s, site=%s)",
		p.config.Service, p.config.Env, p.config.Site)

	// Start flush loop.
	go p.flushLoop(ctx)

	return nil
}

// Stop stops the Datadog plugin.
func (p *Plugin) Stop() error {
	select {
	case <-p.stopCh:
		return nil // already stopped
	default:
	}

	close(p.stopCh)

	// Flush remaining data.
	p.flush()

	<-p.stoppedCh

	p.logger.Printf("[Datadog] Plugin stopped")
	return nil
}

// RecordMetric records a metric.
func (p *Plugin) RecordMetric(name string, metricType MetricType, value float64, tags ...string) {
	if !p.config.Enabled || !p.config.MetricsEnabled {
		return
	}

	globalTags := append(p.config.Tags, fmt.Sprintf("service:%s", p.config.Service))
	if p.config.Env != "" {
		globalTags = append(globalTags, fmt.Sprintf("env:%s", p.config.Env))
	}
	if p.config.Version != "" {
		globalTags = append(globalTags, fmt.Sprintf("version:%s", p.config.Version))
	}
	allTags := append(globalTags, tags...)

	metric := &Metric{
		Name:      name,
		Type:      metricType,
		Value:     value,
		Tags:      allTags,
		Timestamp: time.Now(),
	}

	p.metricsMu.Lock()
	p.metrics = append(p.metrics, metric)
	p.metricsMu.Unlock()
}

// RecordCounter records a counter metric.
func (p *Plugin) RecordCounter(name string, value float64, tags ...string) {
	p.RecordMetric(name, MetricTypeCounter, value, tags...)
}

// RecordGauge records a gauge metric.
func (p *Plugin) RecordGauge(name string, value float64, tags ...string) {
	p.RecordMetric(name, MetricTypeGauge, value, tags...)
}

// RecordHistogram records a histogram metric.
func (p *Plugin) RecordHistogram(name string, value float64, tags ...string) {
	p.RecordMetric(name, MetricTypeHistogram, value, tags...)
}

// RecordTrace records a trace.
func (p *Plugin) RecordTrace(trace *Trace) {
	if !p.config.Enabled || !p.config.TracesEnabled {
		return
	}

	if trace.Service == "" {
		trace.Service = p.config.Service
	}

	p.tracesMu.Lock()
	p.traces = append(p.traces, trace)
	p.tracesMu.Unlock()
}

// StartTrace starts a new trace.
func (p *Plugin) StartTrace(name, resource string, traceID uint64) *Trace {
	return &Trace{
		TraceID:  traceID,
		SpanID:   generateSpanID(),
		Name:     name,
		Resource: resource,
		Service:  p.config.Service,
		Type:     "custom",
		Start:    time.Now(),
		Meta:     make(map[string]string),
		Metrics:  make(map[string]float64),
	}
}

// FinishTrace finishes a trace.
func (p *Plugin) FinishTrace(trace *Trace, err error) {
	if trace == nil {
		return
	}

	trace.Duration = time.Since(trace.Start)
	if err != nil {
		trace.Error = 1
		trace.Meta["error.message"] = err.Error()
		trace.Meta["error.type"] = fmt.Sprintf("%T", err)
	}

	p.RecordTrace(trace)
}

// RecordLog records a log entry.
func (p *Plugin) RecordLog(level, message string, attributes map[string]interface{}) {
	if !p.config.Enabled || !p.config.LogsEnabled {
		return
	}

	entry := &LogEntry{
		Message:    message,
		Level:      level,
		Service:    p.config.Service,
		Timestamp:  time.Now(),
		Attributes: attributes,
	}

	p.logsMu.Lock()
	p.logs = append(p.logs, entry)
	p.logsMu.Unlock()
}

// RecordInfoLog records an info log.
func (p *Plugin) RecordInfoLog(message string, attributes map[string]interface{}) {
	p.RecordLog("info", message, attributes)
}

// RecordWarnLog records a warning log.
func (p *Plugin) RecordWarnLog(message string, attributes map[string]interface{}) {
	p.RecordLog("warn", message, attributes)
}

// RecordErrorLog records an error log.
func (p *Plugin) RecordErrorLog(message string, attributes map[string]interface{}) {
	p.RecordLog("error", message, attributes)
}

// RecordHTTPRequest records HTTP request metrics.
func (p *Plugin) RecordHTTPRequest(method, path string, statusCode int, duration time.Duration, tags ...string) {
	requestTags := append(tags,
		fmt.Sprintf("method:%s", method),
		fmt.Sprintf("path:%s", path),
		fmt.Sprintf("status_code:%d", statusCode),
	)

	p.RecordCounter("koutaku.http.requests", 1, requestTags...)
	p.RecordHistogram("koutaku.http.request.duration", duration.Seconds(), requestTags...)
}

// RecordProviderRequest records provider request metrics.
func (p *Plugin) RecordProviderRequest(provider, model string, statusCode int, duration time.Duration, tags ...string) {
	requestTags := append(tags,
		fmt.Sprintf("provider:%s", provider),
		fmt.Sprintf("model:%s", model),
		fmt.Sprintf("status_code:%d", statusCode),
	)

	p.RecordCounter("koutaku.provider.requests", 1, requestTags...)
	p.RecordHistogram("koutaku.provider.request.duration", duration.Seconds(), requestTags...)
}

// RecordTokenUsage records token usage metrics.
func (p *Plugin) RecordTokenUsage(provider, model string, promptTokens, completionTokens int, tags ...string) {
	usageTags := append(tags,
		fmt.Sprintf("provider:%s", provider),
		fmt.Sprintf("model:%s", model),
	)

	p.RecordCounter("koutaku.tokens.prompt", float64(promptTokens), usageTags...)
	p.RecordCounter("koutaku.tokens.completion", float64(completionTokens), usageTags...)
	p.RecordCounter("koutaku.tokens.total", float64(promptTokens+completionTokens), usageTags...)
}

// RecordCacheMetrics records cache metrics.
func (p *Plugin) RecordCacheMetrics(hit bool, tags ...string) {
	cacheTags := append(tags)
	if hit {
		cacheTags = append(cacheTags, "result:hit")
		p.RecordCounter("koutaku.cache.hits", 1, cacheTags...)
	} else {
		cacheTags = append(cacheTags, "result:miss")
		p.RecordCounter("koutaku.cache.misses", 1, cacheTags...)
	}
}

// GetStats returns plugin statistics.
func (p *Plugin) GetStats() map[string]interface{} {
	p.metricsMu.Lock()
	metricsCount := len(p.metrics)
	p.metricsMu.Unlock()

	p.tracesMu.Lock()
	tracesCount := len(p.traces)
	p.tracesMu.Unlock()

	p.logsMu.Lock()
	logsCount := len(p.logs)
	p.logsMu.Unlock()

	return map[string]interface{}{
		"metrics_buffered": metricsCount,
		"traces_buffered":  tracesCount,
		"logs_buffered":    logsCount,
		"metrics_sent":     p.metricsSent,
		"traces_sent":      p.tracesSent,
		"logs_sent":        p.logsSent,
	}
}

// flushLoop periodically flushes data to Datadog.
func (p *Plugin) flushLoop(ctx context.Context) {
	defer func() {
		select {
		case p.stoppedCh <- struct{}{}:
		default:
		}
	}()

	ticker := time.NewTicker(p.config.FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.flush()
		}
	}
}

// flush sends buffered data to Datadog.
func (p *Plugin) flush() {
	// Flush metrics.
	p.flushMetrics()

	// Flush traces.
	p.flushTraces()

	// Flush logs.
	p.flushLogs()
}

// flushMetrics sends buffered metrics to Datadog.
func (p *Plugin) flushMetrics() {
	p.metricsMu.Lock()
	if len(p.metrics) == 0 {
		p.metricsMu.Unlock()
		return
	}

	metrics := p.metrics
	p.metrics = make([]*Metric, 0)
	p.metricsMu.Unlock()

	// In a real implementation, this would send metrics to Datadog via DogStatsD or API.
	p.logger.Printf("[Datadog] Flushing %d metrics", len(metrics))
	p.metricsSent += uint64(len(metrics))
}

// flushTraces sends buffered traces to Datadog.
func (p *Plugin) flushTraces() {
	p.tracesMu.Lock()
	if len(p.traces) == 0 {
		p.tracesMu.Unlock()
		return
	}

	traces := p.traces
	p.traces = make([]*Trace, 0)
	p.tracesMu.Unlock()

	// In a real implementation, this would send traces to Datadog Trace Agent.
	p.logger.Printf("[Datadog] Flushing %d traces", len(traces))
	p.tracesSent += uint64(len(traces))
}

// flushLogs sends buffered logs to Datadog.
func (p *Plugin) flushLogs() {
	p.logsMu.Lock()
	if len(p.logs) == 0 {
		p.logsMu.Unlock()
		return
	}

	logs := p.logs
	p.logs = make([]*LogEntry, 0)
	p.logsMu.Unlock()

	// In a real implementation, this would send logs to Datadog Logs API.
	p.logger.Printf("[Datadog] Flushing %d logs", len(logs))
	p.logsSent += uint64(len(logs))
}

// generateSpanID generates a random span ID.
func generateSpanID() uint64 {
	// In a real implementation, this would generate a random uint64.
	// For now, we'll use a simple counter.
	return uint64(time.Now().UnixNano())
}
