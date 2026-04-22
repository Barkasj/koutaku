// Package guardrails provides content safety and policy enforcement for
// the Koutaku AI Gateway using CEL expressions and multi-provider validation.
package guardrails

import "time"

// ProviderType identifies a guardrail validation backend.
type ProviderType string

const (
	ProviderBedrock  ProviderType = "bedrock"
	ProviderAzure    ProviderType = "azure"
	ProviderGraySwan ProviderType = "grayswan"
	ProviderPatronus ProviderType = "patronus"
)

// Stage indicates whether validation runs on input or output.
type Stage string

const (
	StageInput  Stage = "input"
	StageOutput Stage = "output"
)

// Severity classifies how serious a violation is.
type Severity string

const (
	SeverityLow      Severity = "low"
	SeverityMedium   Severity = "medium"
	SeverityHigh     Severity = "high"
	SeverityCritical Severity = "critical"
)

// Rule is a single guardrail rule evaluated via a CEL expression.
type Rule struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Expression  string       `json:"expression"` // CEL expression
	Stage       Stage        `json:"stage"`      // input or output
	Severity    Severity     `json:"severity"`
	Enabled     bool         `json:"enabled"`
	Action      string       `json:"action"` // "block", "flag", "log"
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
}

// Profile groups rules and provider configurations.
type Profile struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	RuleIDs     []string       `json:"rule_ids"`
	Providers   []ProviderType `json:"providers"`
	Enabled     bool           `json:"enabled"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

// Violation records a single rule breach.
type Violation struct {
	RuleID      string   `json:"rule_id"`
	RuleName    string   `json:"rule_name"`
	Stage       Stage    `json:"stage"`
	Severity    Severity `json:"severity"`
	Action      string   `json:"action"`
	Message     string   `json:"message"`
	Expression  string   `json:"expression"`
	EvalResult  string   `json:"eval_result,omitempty"`
}

// ValidationResult is the outcome of running all applicable rules.
type ValidationResult struct {
	Passed     bool        `json:"passed"`
	Violations []Violation `json:"violations,omitempty"`
	Duration   time.Duration `json:"duration_ns"`
}

// ProviderConfig holds connection details for a specific provider.
type ProviderConfig struct {
	Type     ProviderType  `json:"type"`
	Endpoint string        `json:"endpoint"`
	APIKey   string        `json:"api_key,omitempty"`
	Timeout  time.Duration `json:"timeout,omitempty"`
	Enabled  bool          `json:"enabled"`
}

// Config is the top-level guardrails plugin configuration.
type Config struct {
	Providers []ProviderConfig `json:"providers"`
	Profiles  []Profile        `json:"profiles"`
	Rules     []Rule           `json:"rules"`
}
