package guardrails

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Plugin orchestrates guardrail rule evaluation and provider validation.
type Plugin struct {
	mu        sync.RWMutex
	evaluator *CELEvaluator
	rules     map[string]*Rule      // ruleID -> Rule
	profiles  map[string]*Profile   // profileID -> Profile
	providers map[ProviderType]Provider
}

// NewPlugin creates a guardrails plugin with the given configuration.
// All providers are initialised; disabled ones are skipped.
func NewPlugin(cfg Config) (*Plugin, error) {
	evaluator, err := NewCELEvaluator()
	if err != nil {
		return nil, err
	}

	p := &Plugin{
		evaluator: evaluator,
		rules:     make(map[string]*Rule),
		profiles:  make(map[string]*Profile),
		providers: make(map[ProviderType]Provider),
	}

	for _, pc := range cfg.Providers {
		if !pc.Enabled {
			continue
		}
		prov, err := NewProvider(pc)
		if err != nil {
			return nil, fmt.Errorf("guardrails: provider init: %w", err)
		}
		p.providers[pc.Type] = prov
	}

	for i := range cfg.Rules {
		r := cfg.Rules[i]
		if r.ID == "" {
			r.ID = uuid.NewString()
		}
		p.rules[r.ID] = &r
	}

	for i := range cfg.Profiles {
		prof := cfg.Profiles[i]
		if prof.ID == "" {
			prof.ID = uuid.NewString()
		}
		p.profiles[prof.ID] = &prof
	}

	return p, nil
}

// GetName returns the plugin name.
func (p *Plugin) GetName() string { return "guardrails" }

// Cleanup releases resources.
func (p *Plugin) Cleanup() error { return nil }

// --- Rule management ---

// AddRule validates and stores a new rule.
func (p *Plugin) AddRule(rule Rule) (*Rule, error) {
	if err := p.evaluator.Validate(rule.Expression); err != nil {
		return nil, err
	}
	if rule.ID == "" {
		rule.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	rule.CreatedAt = now
	rule.UpdatedAt = now

	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules[rule.ID] = &rule
	return &rule, nil
}

// GetRule returns a rule by ID.
func (p *Plugin) GetRule(id string) (*Rule, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	r, ok := p.rules[id]
	if !ok {
		return nil, fmt.Errorf("guardrails: rule %q not found", id)
	}
	cp := *r
	return &cp, nil
}

// ListRules returns all rules.
func (p *Plugin) ListRules() []Rule {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]Rule, 0, len(p.rules))
	for _, r := range p.rules {
		out = append(out, *r)
	}
	return out
}

// UpdateRule replaces an existing rule.
func (p *Plugin) UpdateRule(rule Rule) error {
	if err := p.evaluator.Validate(rule.Expression); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.rules[rule.ID]; !ok {
		return fmt.Errorf("guardrails: rule %q not found", rule.ID)
	}
	rule.UpdatedAt = time.Now().UTC()
	p.rules[rule.ID] = &rule
	return nil
}

// DeleteRule removes a rule.
func (p *Plugin) DeleteRule(id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.rules[id]; !ok {
		return fmt.Errorf("guardrails: rule %q not found", id)
	}
	delete(p.rules, id)
	return nil
}

// --- Profile management ---

// AddProfile creates a new profile.
func (p *Plugin) AddProfile(prof Profile) *Profile {
	if prof.ID == "" {
		prof.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	prof.CreatedAt = now
	prof.UpdatedAt = now

	p.mu.Lock()
	defer p.mu.Unlock()
	p.profiles[prof.ID] = &prof
	return &prof
}

// GetProfile returns a profile by ID.
func (p *Plugin) GetProfile(id string) (*Profile, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	prof, ok := p.profiles[id]
	if !ok {
		return nil, fmt.Errorf("guardrails: profile %q not found", id)
	}
	cp := *prof
	return &cp, nil
}

// ListProfiles returns all profiles.
func (p *Plugin) ListProfiles() []Profile {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]Profile, 0, len(p.profiles))
	for _, prof := range p.profiles {
		out = append(out, *prof)
	}
	return out
}

// --- Validation ---

// ValidateInput runs all enabled input-stage rules and provider checks.
func (p *Plugin) ValidateInput(ctx context.Context, input string, profileID string, meta map[string]string) (*ValidationResult, error) {
	return p.validate(ctx, StageInput, input, "", profileID, meta)
}

// ValidateOutput runs all enabled output-stage rules and provider checks.
func (p *Plugin) ValidateOutput(ctx context.Context, output string, profileID string, meta map[string]string) (*ValidationResult, error) {
	return p.validate(ctx, StageOutput, "", output, profileID, meta)
}

// Validate runs both input and output validation in sequence.
func (p *Plugin) Validate(ctx context.Context, input, output, profileID string, meta map[string]string) (*ValidationResult, error) {
	start := time.Now()
	inResult, err := p.validate(ctx, StageInput, input, "", profileID, meta)
	if err != nil {
		return nil, err
	}
	outResult, err := p.validate(ctx, StageOutput, "", output, profileID, meta)
	if err != nil {
		return nil, err
	}
	merged := &ValidationResult{
		Passed:   inResult.Passed && outResult.Passed,
		Violations: append(inResult.Violations, outResult.Violations...),
		Duration: time.Since(start),
	}
	return merged, nil
}

func (p *Plugin) validate(ctx context.Context, stage Stage, input, output, profileID string, meta map[string]string) (*ValidationResult, error) {
	start := time.Now()

	p.mu.RLock()
	defer p.mu.RUnlock()

	// Collect rules to evaluate
	var rulesToEval []*Rule
	if profileID != "" {
		prof, ok := p.profiles[profileID]
		if !ok {
			return nil, fmt.Errorf("guardrails: profile %q not found", profileID)
		}
		for _, rid := range prof.RuleIDs {
			if r, ok := p.rules[rid]; ok && r.Enabled && r.Stage == stage {
				rulesToEval = append(rulesToEval, r)
			}
		}
	} else {
		// No profile — evaluate all enabled rules for this stage
		for _, r := range p.rules {
			if r.Enabled && r.Stage == stage {
				rulesToEval = append(rulesToEval, r)
			}
		}
	}

	var violations []Violation
	evalCtx := EvalContext{
		Input:    input,
		Output:   output,
		Metadata: meta,
	}

	// CEL rule evaluation
	for _, rule := range rulesToEval {
		triggered, err := p.evaluator.Evaluate(rule.Expression, evalCtx)
		if err != nil {
			// Log error but continue evaluating other rules
			violations = append(violations, Violation{
				RuleID:  rule.ID,
				RuleName: rule.Name,
				Stage:   stage,
				Severity: rule.Severity,
				Action:  "log",
				Message: fmt.Sprintf("CEL eval error: %v", err),
			})
			continue
		}
		if triggered {
			violations = append(violations, Violation{
				RuleID:     rule.ID,
				RuleName:   rule.Name,
				Stage:      stage,
				Severity:   rule.Severity,
				Action:     rule.Action,
				Message:    rule.Description,
				Expression: rule.Expression,
			})
		}
	}

	// Provider validation (if profile has providers configured)
	if profileID != "" {
		prof := p.profiles[profileID]
		for _, ptype := range prof.Providers {
			prov, ok := p.providers[ptype]
			if !ok {
				continue
			}
			var provViolations []Violation
			var err error
			switch stage {
			case StageInput:
				provViolations, err = prov.ValidateInput(ctx, input, meta)
			case StageOutput:
				provViolations, err = prov.ValidateOutput(ctx, output, meta)
			}
			if err != nil {
				violations = append(violations, Violation{
					RuleID:  string(ptype) + ":error",
					Stage:   stage,
					Message: fmt.Sprintf("provider error: %v", err),
					Action:  "log",
				})
				continue
			}
			violations = append(violations, provViolations...)
		}
	}

	passed := true
	for _, v := range violations {
		if v.Action == "block" {
			passed = false
			break
		}
	}

	return &ValidationResult{
		Passed:     passed,
		Violations: violations,
		Duration:   time.Since(start),
	}, nil
}
