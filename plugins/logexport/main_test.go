package logexport

import (
	"context"
	"log"
	"os"
	"testing"
	"time"
)

func TestNewPlugin(t *testing.T) {
	config := DefaultConfig()
	config.Enabled = true

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

func TestPluginRecordLog(t *testing.T) {
	config := DefaultConfig()
	config.Enabled = true

	logger := log.New(os.Stdout, "[Test] ", log.LstdFlags)

	plugin := New(config, logger)

	// Record a log entry.
	entry := &LogEntry{
		Timestamp: time.Now(),
		Level:     "info",
		Message:   "Test log message",
		Service:   "test-service",
		Source:    "test",
	}

	plugin.RecordLog(entry)

	// Check that log is buffered.
	plugin.bufferMu.Lock()
	if len(plugin.buffer) != 1 {
		t.Errorf("Expected 1 log in buffer, got %d", len(plugin.buffer))
	}
	plugin.bufferMu.Unlock()
}

func TestPluginRecordHTTPRequestLog(t *testing.T) {
	config := DefaultConfig()
	config.Enabled = true

	logger := log.New(os.Stdout, "[Test] ", log.LstdFlags)

	plugin := New(config, logger)

	// Record HTTP request log.
	plugin.RecordHTTPRequestLog("GET", "/api/test", 200, 100*time.Millisecond, "req-123", map[string]interface{}{"key": "value"})

	// Check that log is buffered.
	plugin.bufferMu.Lock()
	if len(plugin.buffer) != 1 {
		t.Errorf("Expected 1 log in buffer, got %d", len(plugin.buffer))
	}
	plugin.bufferMu.Unlock()
}

func TestPluginRecordProviderLog(t *testing.T) {
	config := DefaultConfig()
	config.Enabled = true

	logger := log.New(os.Stdout, "[Test] ", log.LstdFlags)

	plugin := New(config, logger)

	// Record provider log.
	plugin.RecordProviderLog("openai", "gpt-4", 200, 500*time.Millisecond, "req-123", nil)

	// Check that log is buffered.
	plugin.bufferMu.Lock()
	if len(plugin.buffer) != 1 {
		t.Errorf("Expected 1 log in buffer, got %d", len(plugin.buffer))
	}
	plugin.bufferMu.Unlock()
}

func TestPluginRecordAuthLog(t *testing.T) {
	config := DefaultConfig()
	config.Enabled = true

	logger := log.New(os.Stdout, "[Test] ", log.LstdFlags)

	plugin := New(config, logger)

	// Record auth log.
	plugin.RecordAuthLog("user-123", "login", true, "req-123")

	// Check that log is buffered.
	plugin.bufferMu.Lock()
	if len(plugin.buffer) != 1 {
		t.Errorf("Expected 1 log in buffer, got %d", len(plugin.buffer))
	}
	plugin.bufferMu.Unlock()
}

func TestPluginGetStats(t *testing.T) {
	config := DefaultConfig()
	config.Enabled = true

	logger := log.New(os.Stdout, "[Test] ", log.LstdFlags)

	plugin := New(config, logger)

	// Record some logs.
	plugin.RecordLog(&LogEntry{Timestamp: time.Now(), Level: "info", Message: "test1"})
	plugin.RecordLog(&LogEntry{Timestamp: time.Now(), Level: "info", Message: "test2"})

	stats := plugin.GetStats()

	if stats["buffer_size"] != 2 {
		t.Errorf("Expected buffer size 2, got %v", stats["buffer_size"])
	}
}

func TestDefaultConfig(t *testing.T) {
	config := DefaultConfig()

	if config.Enabled {
		t.Error("Expected plugin to be disabled by default")
	}

	if config.BufferSize != 10000 {
		t.Errorf("Expected buffer size 10000, got %d", config.BufferSize)
	}

	if config.FlushInterval != 5*time.Second {
		t.Errorf("Expected flush interval 5s, got %v", config.FlushInterval)
	}

	if config.BatchSize != 100 {
		t.Errorf("Expected batch size 100, got %d", config.BatchSize)
	}

	if config.MaxRetries != 3 {
		t.Errorf("Expected max retries 3, got %d", config.MaxRetries)
	}

	if config.RetryDelay != 1*time.Second {
		t.Errorf("Expected retry delay 1s, got %v", config.RetryDelay)
	}
}

func TestExportTypes(t *testing.T) {
	tests := []struct {
		exportType ExportType
		expected   string
	}{
		{ExportTypeElasticsearch, "elasticsearch"},
		{ExportTypeSplunk, "splunk"},
		{ExportTypeCloudWatch, "cloudwatch"},
		{ExportTypeHTTP, "http"},
		{ExportTypeSyslog, "syslog"},
	}

	for _, tt := range tests {
		if string(tt.exportType) != tt.expected {
			t.Errorf("Expected export type '%s', got '%s'", tt.expected, string(tt.exportType))
		}
	}
}

func TestGetLogLevelFromStatus(t *testing.T) {
	tests := []struct {
		statusCode int
		expected   string
	}{
		{200, "info"},
		{301, "info"},
		{400, "warn"},
		{404, "warn"},
		{500, "error"},
		{502, "error"},
	}

	for _, tt := range tests {
		result := getLogLevelFromStatus(tt.statusCode)
		if result != tt.expected {
			t.Errorf("For status %d, expected level '%s', got '%s'", tt.statusCode, tt.expected, result)
		}
	}
}
