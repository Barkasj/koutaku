package auditlog

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const PluginName = "auditlog"

// Plugin implements the Koutaku audit log plugin.
type Plugin struct {
	mu     sync.RWMutex
	store  AuditLogStore
	config Config
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates an AuditLogPlugin backed by the given store.
// If store is nil, an in-memory store is used.
func New(store AuditLogStore, cfg Config) *Plugin {
	if store == nil {
		store = NewMemoryStore()
	}
	return &Plugin{
		store:  store,
		config: cfg,
	}
}

// GetName returns the plugin name.
func (p *Plugin) GetName() string { return PluginName }

// Cleanup stops background workers and flushes pending exports.
func (p *Plugin) Cleanup() error {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
	return nil
}

// --- Core API ---

// Record creates, signs, persists, and exports an audit event.
func (p *Plugin) Record(
	ctx context.Context,
	eventType EventType,
	severity Severity,
	actor Actor,
	details EventDetails,
) (*AuditEvent, error) {
	event := &AuditEvent{
		ID:        uuid.NewString(),
		Timestamp: time.Now().UTC(),
		EventType: eventType,
		Severity:  severity,
		Actor:     actor,
		Details:   details,
	}
	event.Signature = p.sign(event)

	if err := p.store.Append(event); err != nil {
		return nil, fmt.Errorf("auditlog append: %w", err)
	}

	// Async SIEM export
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.exportToSIEM(event)
	}()

	return event, nil
}

// Query retrieves events matching the filter.
func (p *Plugin) Query(filter QueryFilter) ([]AuditEvent, error) {
	return p.store.Query(filter)
}

// Count returns the number of matching events.
func (p *Plugin) Count(filter QueryFilter) (int64, error) {
	return p.store.Count(filter)
}

// Verify checks the HMAC signature of an event.
func (p *Plugin) Verify(event *AuditEvent) bool {
	if event == nil {
		return false
	}
	expected := p.sign(event)
	return hmac.Equal([]byte(event.Signature), []byte(expected))
}

// --- Signing ---

func (p *Plugin) sign(e *AuditEvent) string {
	payload := fmt.Sprintf("%s|%s|%s|%s|%s",
		e.ID, e.Timestamp.Format(time.RFC3339Nano),
		e.EventType, e.Actor.ID, e.Details.Action)
	mac := hmac.New(sha256.New, []byte(p.config.HMACKey))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// --- Retention cleanup ---

// StartCleanup launches a background goroutine that purges expired events
// every interval. It respects the context cancellation.
func (p *Plugin) StartCleanup(ctx context.Context, interval time.Duration) {
	if p.config.RetentionDays <= 0 {
		return
	}
	ctx, p.cancel = context.WithCancel(ctx)
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cutoff := time.Now().UTC().AddDate(0, 0, -p.config.RetentionDays)
				_, _ = p.store.Purge(cutoff)
			}
		}
	}()
}

// --- SIEM export ---

func (p *Plugin) exportToSIEM(event *AuditEvent) {
	p.mu.RLock()
	siems := make([]SIEMConfig, len(p.config.SIEM))
	copy(siems, p.config.SIEM)
	p.mu.RUnlock()

	body, err := json.Marshal(event)
	if err != nil {
		return
	}

	for _, s := range siems {
		if !s.Enabled || s.Endpoint == "" {
			continue
		}
		switch strings.ToLower(s.Backend) {
		case "splunk":
			p.sendToSplunk(s, body)
		case "datadog":
			p.sendToDatadog(s, body)
		case "elastic":
			p.sendToElastic(s, body)
		case "webhook":
			p.sendToWebhook(s, body)
		}
	}
}

func (p *Plugin) sendToSplunk(cfg SIEMConfig, body []byte) {
	// Splunk HEC format: wrap event in {"event": <payload>}
	hec := fmt.Sprintf(`{"event":%s}`, body)
	p.httpPost(cfg.Endpoint, cfg.Token, "application/json", []byte(hec))
}

func (p *Plugin) sendToDatadog(cfg SIEMConfig, body []byte) {
	p.httpPost(cfg.Endpoint, cfg.Token, "application/json", body)
}

func (p *Plugin) sendToElastic(cfg SIEMConfig, body []byte) {
	p.httpPost(cfg.Endpoint, cfg.Token, "application/json", body)
}

func (p *Plugin) sendToWebhook(cfg SIEMConfig, body []byte) {
	p.httpPost(cfg.Endpoint, cfg.Token, "application/json", body)
}

func (p *Plugin) httpPost(url, token, contentType string, body []byte) {
	req, err := http.NewRequest(http.MethodPost, url, io.NopCloser(strings.NewReader(string(body))))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", contentType)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// --- Convenience event constructors ---

// RecordAuthEvent records an authentication event.
func (p *Plugin) RecordAuthEvent(ctx context.Context, actor Actor, action string, meta map[string]string) (*AuditEvent, error) {
	return p.Record(ctx, EventAuth, SeverityInfo, actor, EventDetails{
		Action:   action,
		Metadata: meta,
	})
}

// RecordConfigChangeEvent records a configuration change.
func (p *Plugin) RecordConfigChangeEvent(ctx context.Context, actor Actor, resourceType, resourceID, action string, before, after map[string]any) (*AuditEvent, error) {
	return p.Record(ctx, EventConfigChange, SeverityWarning, actor, EventDetails{
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		Before:       before,
		After:        after,
	})
}

// RecordSecurityEvent records a security-related event.
func (p *Plugin) RecordSecurityEvent(ctx context.Context, actor Actor, action, description string, severity Severity) (*AuditEvent, error) {
	return p.Record(ctx, EventSecurity, severity, actor, EventDetails{
		Action:      action,
		Description: description,
	})
}

// RecordDataAccessEvent records a data access event.
func (p *Plugin) RecordDataAccessEvent(ctx context.Context, actor Actor, resourceType, resourceID, action string) (*AuditEvent, error) {
	return p.Record(ctx, DataAccess, SeverityInfo, actor, EventDetails{
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
	})
}

// --- HTTP handler for query API ---

// Handler returns an http.Handler that exposes the audit log query API.
//
//	GET  /api/audit-logs       — query events (JSON body or query params)
//	GET  /api/audit-logs/count — count matching events
//	POST /api/audit-logs       — record a new event
func (p *Plugin) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/audit-logs", p.handleQuery)
	mux.HandleFunc("/api/audit-logs/count", p.handleCount)
	return mux
}

func (p *Plugin) handleQuery(w http.ResponseWriter, r *http.Request) {
	var filter QueryFilter
	if r.Method == http.MethodGet {
		q := r.URL.Query()
		if v := q.Get("actor_id"); v != "" {
			filter.ActorID = v
		}
		if v := q.Get("actor_type"); v != "" {
			filter.ActorType = v
		}
		if v := q.Get("action"); v != "" {
			filter.Action = v
		}
		if v := q.Get("limit"); v != "" {
			fmt.Sscanf(v, "%d", &filter.Limit)
		}
	} else {
		json.NewDecoder(r.Body).Decode(&filter)
	}
	events, err := p.Query(filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(events)
}

func (p *Plugin) handleCount(w http.ResponseWriter, r *http.Request) {
	var filter QueryFilter
	json.NewDecoder(r.Body).Decode(&filter)
	count, err := p.Count(filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int64{"count": count})
}

// DataAccess is a package-level constant alias for EventDataAccess used
// by the convenience constructor.
const DataAccess = EventDataAccess
