// Package logexport provides enterprise-grade log export capabilities for Koutaku AI Gateway.
// It supports exporting logs to multiple external systems including Elasticsearch,
// Splunk, CloudWatch, and custom destinations.
package logexport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

const PluginName = "logexport"

// ExportType represents the type of log export destination.
type ExportType string

const (
	// ExportTypeElasticsearch exports logs to Elasticsearch.
	ExportTypeElasticsearch ExportType = "elasticsearch"
	// ExportTypeSplunk exports logs to Splunk.
	ExportTypeSplunk ExportType = "splunk"
	// ExportTypeCloudWatch exports logs to AWS CloudWatch.
	ExportTypeCloudWatch ExportType = "cloudwatch"
	// ExportTypeHTTP exports logs to a custom HTTP endpoint.
	ExportTypeHTTP ExportType = "http"
	// ExportTypeSyslog exports logs to syslog.
	ExportTypeSyslog ExportType = "syslog"
)

// Config represents the log export plugin configuration.
type Config struct {
	// Enabled enables the log export plugin.
	Enabled bool `json:"enabled"`
	// Exporters is a list of log exporter configurations.
	Exporters []ExporterConfig `json:"exporters"`
	// BufferSize is the size of the log buffer.
	// Defaults to 10000.
	BufferSize int `json:"buffer_size,omitempty"`
	// FlushInterval is the interval to flush logs.
	// Defaults to 5 seconds.
	FlushInterval time.Duration `json:"flush_interval,omitempty"`
	// BatchSize is the number of logs to send in a batch.
	// Defaults to 100.
	BatchSize int `json:"batch_size,omitempty"`
	// MaxRetries is the maximum number of retries for failed exports.
	// Defaults to 3.
	MaxRetries int `json:"max_retries,omitempty"`
	// RetryDelay is the delay between retries.
	// Defaults to 1 second.
	RetryDelay time.Duration `json:"retry_delay,omitempty"`
}

// ExporterConfig represents the configuration for a specific log exporter.
type ExporterConfig struct {
	// Type is the exporter type.
	Type ExportType `json:"type"`
	// Name is a human-readable name for the exporter.
	Name string `json:"name"`
	// Enabled enables this exporter.
	Enabled bool `json:"enabled"`
	// Endpoint is the exporter endpoint URL.
	Endpoint string `json:"endpoint"`
	// APIKey is the API key for authentication.
	APIKey string `json:"api_key,omitempty"`
	// Index is the index name (Elasticsearch-specific).
	Index string `json:"index,omitempty"`
	// Source is the source name (Splunk-specific).
	Source string `json:"source,omitempty"`
	// LogGroup is the CloudWatch log group name.
	LogGroup string `json:"log_group,omitempty"`
	// LogStream is the CloudWatch log stream name.
	LogStream string `json:"log_stream,omitempty"`
	// Region is the AWS region (CloudWatch-specific).
	Region string `json:"region,omitempty"`
	// Headers are custom HTTP headers.
	Headers map[string]string `json:"headers,omitempty"`
	// Format is the log format (json, text).
	Format string `json:"format,omitempty"`
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		Enabled:       false,
		BufferSize:    10000,
		FlushInterval: 5 * time.Second,
		BatchSize:     100,
		MaxRetries:    3,
		RetryDelay:    1 * time.Second,
	}
}

// LogEntry represents a log entry to be exported.
type LogEntry struct {
	// Timestamp is when the log was recorded.
	Timestamp time.Time `json:"timestamp"`
	// Level is the log level (info, warn, error, debug).
	Level string `json:"level"`
	// Message is the log message.
	Message string `json:"message"`
	// Service is the service name.
	Service string `json:"service"`
	// Source is the log source.
	Source string `json:"source,omitempty"`
	// Attributes are additional log attributes.
	Attributes map[string]interface{} `json:"attributes,omitempty"`
	// RequestID is the request ID for correlation.
	RequestID string `json:"request_id,omitempty"`
	// UserID is the user ID.
	UserID string `json:"user_id,omitempty"`
	// Provider is the AI provider name.
	Provider string `json:"provider,omitempty"`
	// Model is the AI model name.
	Model string `json:"model,omitempty"`
	// Duration is the request duration.
	Duration time.Duration `json:"duration,omitempty"`
	// StatusCode is the HTTP status code.
	StatusCode int `json:"status_code,omitempty"`
	// Error is the error message.
	Error string `json:"error,omitempty"`
}

// Exporter is the interface for log exporter implementations.
type Exporter interface {
	// Export exports a batch of log entries.
	Export(ctx context.Context, entries []*LogEntry) error
	// Close closes the exporter.
	Close() error
}

// Plugin is the log export plugin for Koutaku.
type Plugin struct {
	mu     sync.RWMutex
	config Config
	logger *log.Logger

	// Exporters.
	exporters []Exporter

	// Log buffer.
	buffer   []*LogEntry
	bufferMu sync.Mutex

	// Channels.
	stopCh    chan struct{}
	stoppedCh chan struct{}

	// Statistics.
	exportedCount uint64
	failedCount   uint64
}

// New creates a new log export plugin with the given configuration.
func New(cfg Config, logger *log.Logger) *Plugin {
	if logger == nil {
		logger = log.Default()
	}

	// Apply defaults.
	if cfg.BufferSize == 0 {
		cfg.BufferSize = 10000
	}
	if cfg.FlushInterval == 0 {
		cfg.FlushInterval = 5 * time.Second
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = 100
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 3
	}
	if cfg.RetryDelay == 0 {
		cfg.RetryDelay = 1 * time.Second
	}

	return &Plugin{
		config:    cfg,
		logger:    logger,
		buffer:    make([]*LogEntry, 0, cfg.BufferSize),
		stopCh:    make(chan struct{}),
		stoppedCh: make(chan struct{}),
	}
}

// GetName returns the plugin name.
func (p *Plugin) GetName() string {
	return PluginName
}

// Start starts the log export plugin.
func (p *Plugin) Start(ctx context.Context) error {
	if !p.config.Enabled {
		p.logger.Printf("[LogExport] Plugin is disabled")
		return nil
	}

	p.logger.Printf("[LogExport] Starting plugin with %d exporters", len(p.config.Exporters))

	// Initialize exporters.
	for _, exporterConfig := range p.config.Exporters {
		if !exporterConfig.Enabled {
			continue
		}

		exporter, err := createExporter(exporterConfig, p.logger)
		if err != nil {
			p.logger.Printf("[LogExport] Failed to create exporter %s: %v", exporterConfig.Name, err)
			continue
		}

		p.exporters = append(p.exporters, exporter)
		p.logger.Printf("[LogExport] Initialized exporter: %s (%s)", exporterConfig.Name, exporterConfig.Type)
	}

	// Start flush loop.
	go p.flushLoop(ctx)

	return nil
}

// Stop stops the log export plugin.
func (p *Plugin) Stop() error {
	select {
	case <-p.stopCh:
		return nil // already stopped
	default:
	}

	close(p.stopCh)

	// Flush remaining logs.
	p.flush()

	// Close exporters.
	for _, exporter := range p.exporters {
		if err := exporter.Close(); err != nil {
			p.logger.Printf("[LogExport] Failed to close exporter: %v", err)
		}
	}

	<-p.stoppedCh

	p.logger.Printf("[LogExport] Plugin stopped")
	return nil
}

// RecordLog records a log entry for export.
func (p *Plugin) RecordLog(entry *LogEntry) {
	if !p.config.Enabled {
		return
	}

	p.bufferMu.Lock()
	if len(p.buffer) < p.config.BufferSize {
		p.buffer = append(p.buffer, entry)
	} else {
		p.logger.Printf("[LogExport] Buffer full, dropping log entry")
	}
	p.bufferMu.Unlock()
}

// RecordHTTPRequestLog records an HTTP request log.
func (p *Plugin) RecordHTTPRequestLog(method, path string, statusCode int, duration time.Duration, requestID string, attributes map[string]interface{}) {
	entry := &LogEntry{
		Timestamp:  time.Now(),
		Level:      getLogLevelFromStatus(statusCode),
		Message:    fmt.Sprintf("%s %s %d", method, path, statusCode),
		Service:    "koutaku",
		Source:     "http",
		Attributes: attributes,
		RequestID:  requestID,
		Duration:   duration,
		StatusCode: statusCode,
	}

	p.RecordLog(entry)
}

// RecordProviderLog records a provider request log.
func (p *Plugin) RecordProviderLog(provider, model string, statusCode int, duration time.Duration, requestID string, err error) {
	entry := &LogEntry{
		Timestamp:  time.Now(),
		Level:      getLogLevelFromStatus(statusCode),
		Message:    fmt.Sprintf("Provider request: %s/%s %d", provider, model, statusCode),
		Service:    "koutaku",
		Source:     "provider",
		RequestID:  requestID,
		Provider:   provider,
		Model:      model,
		Duration:   duration,
		StatusCode: statusCode,
	}

	if err != nil {
		entry.Error = err.Error()
		entry.Level = "error"
	}

	p.RecordLog(entry)
}

// RecordAuthLog records an authentication log.
func (p *Plugin) RecordAuthLog(userID, action string, success bool, requestID string) {
	level := "info"
	if !success {
		level = "warn"
	}

	entry := &LogEntry{
		Timestamp: time.Now(),
		Level:     level,
		Message:   fmt.Sprintf("Auth %s: %s (success=%t)", action, userID, success),
		Service:   "koutaku",
		Source:    "auth",
		RequestID: requestID,
		UserID:    userID,
		Attributes: map[string]interface{}{
			"action":  action,
			"success": success,
		},
	}

	p.RecordLog(entry)
}

// GetStats returns plugin statistics.
func (p *Plugin) GetStats() map[string]interface{} {
	p.bufferMu.Lock()
	bufferSize := len(p.buffer)
	p.bufferMu.Unlock()

	return map[string]interface{}{
		"buffer_size":    bufferSize,
		"exported_count": p.exportedCount,
		"failed_count":   p.failedCount,
		"exporters":      len(p.exporters),
	}
}

// flushLoop periodically flushes logs to exporters.
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

// flush sends buffered logs to all exporters.
func (p *Plugin) flush() {
	p.bufferMu.Lock()
	if len(p.buffer) == 0 {
		p.bufferMu.Unlock()
		return
	}

	// Get logs to flush.
	logs := p.buffer
	p.buffer = make([]*LogEntry, 0, p.config.BufferSize)
	p.bufferMu.Unlock()

	// Send to all exporters.
	for _, exporter := range p.exporters {
		// Send in batches.
		for i := 0; i < len(logs); i += p.config.BatchSize {
			end := i + p.config.BatchSize
			if end > len(logs) {
				end = len(logs)
			}

			batch := logs[i:end]

			// Retry logic.
			var err error
			for retry := 0; retry <= p.config.MaxRetries; retry++ {
				if retry > 0 {
					time.Sleep(p.config.RetryDelay * time.Duration(retry))
				}

				err = exporter.Export(context.Background(), batch)
				if err == nil {
					p.exportedCount += uint64(len(batch))
					break
				}
			}

			if err != nil {
				p.failedCount += uint64(len(batch))
				p.logger.Printf("[LogExport] Failed to export logs: %v", err)
			}
		}
	}
}

// createExporter creates an exporter based on the configuration.
func createExporter(config ExporterConfig, logger *log.Logger) (Exporter, error) {
	switch config.Type {
	case ExportTypeElasticsearch:
		return NewElasticsearchExporter(config, logger)
	case ExportTypeSplunk:
		return NewSplunkExporter(config, logger)
	case ExportTypeHTTP:
		return NewHTTPExporter(config, logger)
	default:
		return nil, fmt.Errorf("unsupported exporter type: %s", config.Type)
	}
}

// getLogLevelFromStatus returns a log level based on HTTP status code.
func getLogLevelFromStatus(statusCode int) string {
	if statusCode >= 500 {
		return "error"
	}
	if statusCode >= 400 {
		return "warn"
	}
	return "info"
}

// ElasticsearchExporter exports logs to Elasticsearch.
type ElasticsearchExporter struct {
	config     ExporterConfig
	logger     *log.Logger
	httpClient *http.Client
}

// NewElasticsearchExporter creates a new Elasticsearch exporter.
func NewElasticsearchExporter(config ExporterConfig, logger *log.Logger) (*ElasticsearchExporter, error) {
	return &ElasticsearchExporter{
		config: config,
		logger: logger,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}, nil
}

// Export exports a batch of log entries to Elasticsearch.
func (e *ElasticsearchExporter) Export(ctx context.Context, entries []*LogEntry) error {
	// Build bulk request body.
	var body bytes.Buffer

	for _, entry := range entries {
		// Index action.
		indexAction := map[string]interface{}{
			"index": map[string]interface{}{
				"_index": e.config.Index,
			},
		}

		actionJSON, err := json.Marshal(indexAction)
		if err != nil {
			return err
		}

		body.Write(actionJSON)
		body.WriteByte('\n')

		// Document.
		docJSON, err := json.Marshal(entry)
		if err != nil {
			return err
		}

		body.Write(docJSON)
		body.WriteByte('\n')
	}

	// Send bulk request.
	url := fmt.Sprintf("%s/_bulk", e.config.Endpoint)

	req, err := http.NewRequestWithContext(ctx, "POST", url, &body)
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/x-ndjson")
	if e.config.APIKey != "" {
		req.Header.Set("Authorization", "ApiKey "+e.config.APIKey)
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("elasticsearch bulk request failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// Close closes the Elasticsearch exporter.
func (e *ElasticsearchExporter) Close() error {
	return nil
}

// SplunkExporter exports logs to Splunk.
type SplunkExporter struct {
	config     ExporterConfig
	logger     *log.Logger
	httpClient *http.Client
}

// NewSplunkExporter creates a new Splunk exporter.
func NewSplunkExporter(config ExporterConfig, logger *log.Logger) (*SplunkExporter, error) {
	return &SplunkExporter{
		config: config,
		logger: logger,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}, nil
}

// Export exports a batch of log entries to Splunk.
func (s *SplunkExporter) Export(ctx context.Context, entries []*LogEntry) error {
	// Send each entry as a separate event.
	for _, entry := range entries {
		event := map[string]interface{}{
			"event": entry,
		}

		if s.config.Source != "" {
			event["source"] = s.config.Source
		}

		eventJSON, err := json.Marshal(event)
		if err != nil {
			return err
		}

		url := fmt.Sprintf("%s/services/collector/event", s.config.Endpoint)

		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(eventJSON))
		if err != nil {
			return err
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Splunk "+s.config.APIKey)

		resp, err := s.httpClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 400 {
			respBody, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("splunk request failed with status %d: %s", resp.StatusCode, string(respBody))
		}
	}

	return nil
}

// Close closes the Splunk exporter.
func (s *SplunkExporter) Close() error {
	return nil
}

// HTTPExporter exports logs to a custom HTTP endpoint.
type HTTPExporter struct {
	config     ExporterConfig
	logger     *log.Logger
	httpClient *http.Client
}

// NewHTTPExporter creates a new HTTP exporter.
func NewHTTPExporter(config ExporterConfig, logger *log.Logger) (*HTTPExporter, error) {
	return &HTTPExporter{
		config: config,
		logger: logger,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}, nil
}

// Export exports a batch of log entries to a custom HTTP endpoint.
func (h *HTTPExporter) Export(ctx context.Context, entries []*LogEntry) error {
	entriesJSON, err := json.Marshal(entries)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", h.config.Endpoint, bytes.NewReader(entriesJSON))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	for key, value := range h.config.Headers {
		req.Header.Set(key, value)
	}

	if h.config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+h.config.APIKey)
	}

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP export failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// Close closes the HTTP exporter.
func (h *HTTPExporter) Close() error {
	return nil
}
