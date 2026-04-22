// Package auditlog provides an immutable, HMAC-signed audit trail plugin
// for the Koutaku AI Gateway.
package auditlog

import "time"

// EventType categorises the auditable action.
type EventType string

const (
	EventAuth            EventType = "auth"
	EventConfigChange    EventType = "config_change"
	EventSecurity        EventType = "security"
	EventDataAccess      EventType = "data_access"
	EventRoleChange      EventType = "role_change"
	EventKeyRotation     EventType = "key_rotation"
	EventPluginLifecycle EventType = "plugin_lifecycle"
	EventGovernance      EventType = "governance"
)

// Severity indicates the criticality of the event.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// Actor describes who (or what) triggered the event.
type Actor struct {
	Type       string `json:"type"` // "user", "service", "system", "api_key"
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	IPAddress  string `json:"ip_address,omitempty"`
	UserAgent  string `json:"user_agent,omitempty"`
}

// EventDetails carries free-form structured data about the event.
type EventDetails struct {
	Action       string            `json:"action"`
	ResourceType string            `json:"resource_type,omitempty"`
	ResourceID   string            `json:"resource_id,omitempty"`
	Description  string            `json:"description,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	Before       map[string]any    `json:"before,omitempty"` // snapshot before change
	After        map[string]any    `json:"after,omitempty"`  // snapshot after change
}

// AuditEvent is a single immutable log entry.
type AuditEvent struct {
	ID        string       `json:"id"`
	Timestamp time.Time    `json:"timestamp"`
	EventType EventType    `json:"event_type"`
	Severity  Severity     `json:"severity"`
	Actor     Actor        `json:"actor"`
	Details   EventDetails `json:"details"`
	// HMAC-SHA256 signature over (ID + Timestamp + EventType + Actor.ID + Details.Action).
	// Signing key is configurable; the signature guarantees tamper detection.
	Signature string `json:"signature"`
}

// QueryFilter constrains audit log queries.
type QueryFilter struct {
	Start      *time.Time  `json:"start,omitempty"`
	End        *time.Time  `json:"end,omitempty"`
	EventTypes []EventType `json:"event_types,omitempty"`
	Severities []Severity  `json:"severities,omitempty"`
	ActorID    string      `json:"actor_id,omitempty"`
	ActorType  string      `json:"actor_type,omitempty"`
	Action     string      `json:"action,omitempty"`
	Limit      int         `json:"limit,omitempty"`
	Offset     int         `json:"offset,omitempty"`
}

// SIEMConfig holds export settings for a SIEM backend.
type SIEMConfig struct {
	Backend  string            `json:"backend"` // "splunk", "datadog", "elastic", "webhook"
	Endpoint string            `json:"endpoint"`
	Token    string            `json:"token,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Enabled  bool              `json:"enabled"`
}

// Config is the top-level configuration for the audit log plugin.
type Config struct {
	// HMACKey is the secret used to sign audit events. Must be set in production.
	HMACKey string `json:"hmac_key"`
	// RetentionDays controls how long events are kept. 0 = no cleanup.
	RetentionDays int `json:"retention_days"`
	// SIEM backends to forward events to.
	SIEM []SIEMConfig `json:"siem,omitempty"`
}
