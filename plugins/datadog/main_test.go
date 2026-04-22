package datadog

import (
	"context"
	"fmt"
	"log"
	"os"
	"testing"
	"time"
)

func TestNewPlugin(t *testing.T) {
	config := DefaultConfig()
	config.Enabled = true
	config.APIKey = "test-api-key"
	config.Service = "test-service"

	logger := log.New(os.Stdout, "[Test] ", log.LstdFlags)

	plugin := New(config, logger)
	if plugin == nil {
		t.Fatal("Expected non-nil plugin")
	}

	if plugin.GetName() != PluginName {
		t.Errorf("Expected plugin name '%s', got '%s'", PluginName, plugin.GetName())
	}
}

func TestPluginStartStop(t *testing.T) {
	config := DefaultConfig()
	config.Enabled = true
	config.APIKey = "test-api-key"
	config.Service = "test-service"

	logger := log.New(os.Stdout, "[Test] ", log.LstdFlags)

	plugin := New(config, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := plugin.Start(ctx); err != nil {
		t.Fatalf("Failed to start plugin: %v", err)
	}

	// Wait a bit for the plugin to start.
	time.Sleep(100 * time.Millisecond)

	// Stop the plugin.
	if err := plugin.Stop(); err != nil {
		t.Fatalf("Failed to stop plugin: %v", err)
	}
}

func TestPluginRecordMetric(t *testing.T) {
	config := DefaultConfig()
	config.Enabled = true
	config.APIKey = "test-api-key"
	config.Service = "test-service"

	logger := log.New(os.Stdout, "[Test] ", log.LstdFlags)

	plugin := New(config, logger)

	// Record metrics.
	plugin.RecordCounter("test.counter", 1, "tag1:value1")
	plugin.RecordGauge("test.gauge", 42.0, "tag2:value2")
	plugin.RecordHistogram("test.histogram", 0.5, "tag3:value3")

	// Check that metrics are buffered.
	plugin.metricsMu.Lock()
	if len(plugin.metrics) != 3 {
		t.Errorf("Expected 3 metrics, got %d", len(plugin.metrics))
	}
	plugin.metricsMu.Unlock()
}

func TestPluginRecordTrace(t *testing.T) {
	config := DefaultConfig()
	config.Enabled = true
	config.APIKey = "test-api-key"
	config.Service = "test-service"

	logger := log.New(os.Stdout, "[Test] ", log.LstdFlags)

	plugin := New(config, logger)

	// Start and finish a trace.
	trace := plugin.StartTrace("test.operation", "test-resource", 12345)
	if trace == nil {
		t.Fatal("Expected non-nil trace")
	}

	if trace.Name != "test.operation" {
		t.Errorf("Expected trace name 'test.operation', got '%s'", trace.Name)
	}

	if trace.Resource != "test-resource" {
		t.Errorf("Expected trace resource 'test-resource', got '%s'", trace.Resource)
	}

	// Finish the trace.
	plugin.FinishTrace(trace, nil)

	// Check that trace is buffered.
	plugin.tracesMu.Lock()
	if len(plugin.traces) != 1 {
		t.Errorf("Expected 1 trace, got %d", len(plugin.traces))
	}
	plugin.tracesMu.Unlock()
}

func TestPluginRecordTraceWithError(t *testing.T) {
	config := DefaultConfig()
	config.Enabled = true
	config.APIKey = "test-api-key"
	config.Service = "test-service"

	logger := log.New(os.Stdout, "[Test] ", log.LstdFlags)

	plugin := New(config, logger)

	// Start and finish a trace with error.
	trace := plugin.StartTrace("test.operation", "test-resource", 12345)
	plugin.FinishTrace(trace, fmt.Errorf("test error"))

	// Check that trace has error.
	plugin.tracesMu.Lock()
	if len(plugin.traces) != 1 {
		t.Errorf("Expected 1 trace, got %d", len(plugin.traces))
	}

	if plugin.traces[0].Error != 1 {
		t.Error("Expected trace to have error")
	}
	plugin.tracesMu.Unlock()
}

func TestPluginRecordLog(t *testing.T) {
	config := DefaultConfig()
	config.Enabled = true
	config.APIKey = "test-api-key"
	config.Service = "test-service"

	logger := log.New(os.Stdout, "[Test] ", log.LstdFlags)

	plugin := New(config, logger)

	// Record logs.
	plugin.RecordInfoLog("test info message", map[string]interface{}{"key": "value"})
	plugin.RecordWarnLog("test warn message", nil)
	plugin.RecordErrorLog("test error message", map[string]interface{}{"error": "test"})

	// Check that logs are buffered.
	plugin.logsMu.Lock()
	if len(plugin.logs) != 3 {
		t.Errorf("Expected 3 logs, got %d", len(plugin.logs))
	}
	plugin.logsMu.Unlock()
}

func TestPluginRecordHTTPRequest(t *testing.T) {
	config := DefaultConfig()
	config.Enabled = true
	config.APIKey = "test-api-key"
	config.Service = "test-service"

	logger := log.New(os.Stdout, "[Test] ", log.LstdFlags)

	plugin := New(config, logger)

	// Record HTTP request.
	plugin.RecordHTTPRequest("GET", "/api/test", 200, 100*time.Millisecond, "custom:tag")

	// Check that metrics are buffered.
	plugin.metricsMu.Lock()
	if len(plugin.metrics) != 2 {
		t.Errorf("Expected 2 metrics (counter + histogram), got %d", len(plugin.metrics))
	}
	plugin.metricsMu.Unlock()
}

func TestPluginRecordProviderRequest(t *testing.T) {
	config := DefaultConfig()
	config.Enabled = true
	config.APIKey = "test-api-key"
	config.Service = "test-service"

	logger := log.New(os.Stdout, "[Test] ", log.LstdFlags)

	plugin := New(config, logger)

	// Record provider request.
	plugin.RecordProviderRequest("openai", "gpt-4", 200, 500*time.Millisecond, "custom:tag")

	// Check that metrics are buffered.
	plugin.metricsMu.Lock()
	if len(plugin.metrics) != 2 {
		t.Errorf("Expected 2 metrics (counter + histogram), got %d", len(plugin.metrics))
	}
	plugin.metricsMu.Unlock()
}

func TestPluginRecordTokenUsage(t *testing.T) {
	config := DefaultConfig()
	config.Enabled = true
	config.APIKey = "test-api-key"
	config.Service = "test-service"

	logger := log.New(os.Stdout, "[Test] ", log.LstdFlags)

	plugin := New(config, logger)

	// Record token usage.
	plugin.RecordTokenUsage("openai", "gpt-4", 100, 50, "custom:tag")

	// Check that metrics are buffered.
	plugin.metricsMu.Lock()
	if len(plugin.metrics) != 3 {
		t.Errorf("Expected 3 metrics (prompt + completion + total), got %d", len(plugin.metrics))
	}
	plugin.metricsMu.Unlock()
}

func TestPluginRecordCacheMetrics(t *testing.T) {
	config := DefaultConfig()
	config.Enabled = true
	config.APIKey = "test-api-key"
	config.Service = "test-service"

	logger := log.New(os.Stdout, "[Test] ", log.LstdFlags)

	plugin := New(config, logger)

	// Record cache hit.
	plugin.RecordCacheMetrics(true, "custom:tag")

	// Record cache miss.
	plugin.RecordCacheMetrics(false, "custom:tag")

	// Check that metrics are buffered.
	plugin.metricsMu.Lock()
	if len(plugin.metrics) != 2 {
		t.Errorf("Expected 2 metrics, got %d", len(plugin.metrics))
	}
	plugin.metricsMu.Unlock()
}

func TestPluginGetStats(t *testing.T) {
	config := DefaultConfig()
	config.Enabled = true
	config.APIKey = "test-api-key"
	config.Service = "test-service"

	logger := log.New(os.Stdout, "[Test] ", log.LstdFlags)

	plugin := New(config, logger)

	// Record some data.
	plugin.RecordCounter("test.counter", 1)
	trace := plugin.StartTrace("test", "resource", 12345)
	plugin.FinishTrace(trace, nil)
	plugin.RecordInfoLog("test", nil)

	stats := plugin.GetStats()

	if stats["metrics_buffered"] != 1 {
		t.Errorf("Expected 1 metric buffered, got %v", stats["metrics_buffered"])
	}

	if stats["traces_buffered"] != 1 {
		t.Errorf("Expected 1 trace buffered, got %v", stats["traces_buffered"])
	}

	if stats["logs_buffered"] != 1 {
		t.Errorf("Expected 1 log buffered, got %v", stats["logs_buffered"])
	}
}

func TestDefaultConfig(t *testing.T) {
	config := DefaultConfig()

	if config.Enabled {
		t.Error("Expected plugin to be disabled by default")
	}

	if config.Site != "datadoghq.com" {
		t.Errorf("Expected site 'datadoghq.com', got '%s'", config.Site)
	}

	if config.Service != "koutaku" {
		t.Errorf("Expected service 'koutaku', got '%s'", config.Service)
	}

	if !config.MetricsEnabled {
		t.Error("Expected metrics to be enabled by default")
	}

	if !config.TracesEnabled {
		t.Error("Expected traces to be enabled by default")
	}

	if !config.LogsEnabled {
		t.Error("Expected logs to be enabled by default")
	}

	if config.FlushInterval != 10*time.Second {
		t.Errorf("Expected flush interval 10s, got %v", config.FlushInterval)
	}

	if config.StatsdAddr != "localhost:8125" {
		t.Errorf("Expected StatsD address 'localhost:8125', got '%s'", config.StatsdAddr)
	}

	if config.TraceAgentAddr != "localhost:8126" {
		t.Errorf("Expected Trace Agent address 'localhost:8126', got '%s'", config.TraceAgentAddr)
	}
}

func TestMetricTypes(t *testing.T) {
	tests := []struct {
		metricType MetricType
		expected   string
	}{
		{MetricTypeCounter, "counter"},
		{MetricTypeGauge, "gauge"},
		{MetricTypeHistogram, "histogram"},
		{MetricTypeDistribution, "distribution"},
		{MetricTypeSet, "set"},
	}

	for _, tt := range tests {
		if string(tt.metricType) != tt.expected {
			t.Errorf("Expected metric type '%s', got '%s'", tt.expected, string(tt.metricType))
		}
	}
}
