package guardrails

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Provider is the interface that all guardrail validation backends must implement.
type Provider interface {
	// Type returns the provider identifier.
	Type() ProviderType
	// ValidateInput checks the user input for policy violations.
	ValidateInput(ctx context.Context, input string, meta map[string]string) ([]Violation, error)
	// ValidateOutput checks the model output for policy violations.
	ValidateOutput(ctx context.Context, output string, meta map[string]string) ([]Violation, error)
}

// --- Bedrock Guardrails ---

type BedrockProvider struct {
	endpoint string
	apiKey   string
	client   *http.Client
}

func NewBedrockProvider(cfg ProviderConfig) *BedrockProvider {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &BedrockProvider{
		endpoint: cfg.Endpoint,
		apiKey:   cfg.APIKey,
		client:   &http.Client{Timeout: timeout},
	}
}

func (p *BedrockProvider) Type() ProviderType { return ProviderBedrock }

func (p *BedrockProvider) ValidateInput(ctx context.Context, input string, meta map[string]string) ([]Violation, error) {
	return p.validate(ctx, "input", input, meta)
}

func (p *BedrockProvider) ValidateOutput(ctx context.Context, output string, meta map[string]string) ([]Violation, error) {
	return p.validate(ctx, "output", output, meta)
}

func (p *BedrockProvider) validate(ctx context.Context, stage, text string, meta map[string]string) ([]Violation, error) {
	payload := map[string]any{
		"content": []map[string]string{{"text": text}},
		"source":  stage,
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint+"/guardrail/check", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bedrock: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	var result struct {
		Violations []struct {
			RuleID  string `json:"ruleId"`
			Message string `json:"message"`
		} `json:"violations"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("bedrock: unmarshal: %w", err)
	}

	var violations []Violation
	for _, v := range result.Violations {
		violations = append(violations, Violation{
			RuleID:  v.RuleID,
			Stage:   Stage(stage),
			Message: v.Message,
			Action:  "block",
		})
	}
	return violations, nil
}

// --- Azure AI Content Safety ---

type AzureProvider struct {
	endpoint string
	apiKey   string
	client   *http.Client
}

func NewAzureProvider(cfg ProviderConfig) *AzureProvider {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &AzureProvider{
		endpoint: cfg.Endpoint,
		apiKey:   cfg.APIKey,
		client:   &http.Client{Timeout: timeout},
	}
}

func (p *AzureProvider) Type() ProviderType { return ProviderAzure }

func (p *AzureProvider) ValidateInput(ctx context.Context, input string, meta map[string]string) ([]Violation, error) {
	return p.validate(ctx, "input", input)
}

func (p *AzureProvider) ValidateOutput(ctx context.Context, output string, meta map[string]string) ([]Violation, error) {
	return p.validate(ctx, "output", output)
}

func (p *AzureProvider) validate(ctx context.Context, stage, text string) ([]Violation, error) {
	payload := map[string]any{
		"text":      text,
		"blocklist": []string{},
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint+"/contentsafety/text:analyze", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Ocp-Apim-Subscription-Key", p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("azure: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	var result struct {
		CategoriesAnalysis []struct {
			Category string `json:"category"`
			Severity int    `json:"severity"`
		} `json:"categoriesAnalysis"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("azure: unmarshal: %w", err)
	}

	var violations []Violation
	for _, cat := range result.CategoriesAnalysis {
		if cat.Severity > 0 {
			violations = append(violations, Violation{
				RuleID:  "azure:" + cat.Category,
				Stage:   Stage(stage),
				Message: fmt.Sprintf("Azure detected %s with severity %d", cat.Category, cat.Severity),
				Action:  "block",
			})
		}
	}
	return violations, nil
}

// --- GraySwan ---

type GraySwanProvider struct {
	endpoint string
	apiKey   string
	client   *http.Client
}

func NewGraySwanProvider(cfg ProviderConfig) *GraySwanProvider {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &GraySwanProvider{
		endpoint: cfg.Endpoint,
		apiKey:   cfg.APIKey,
		client:   &http.Client{Timeout: timeout},
	}
}

func (p *GraySwanProvider) Type() ProviderType { return ProviderGraySwan }

func (p *GraySwanProvider) ValidateInput(ctx context.Context, input string, meta map[string]string) ([]Violation, error) {
	return p.validate(ctx, "input", input)
}

func (p *GraySwanProvider) ValidateOutput(ctx context.Context, output string, meta map[string]string) ([]Violation, error) {
	return p.validate(ctx, "output", output)
}

func (p *GraySwanProvider) validate(ctx context.Context, stage, text string) ([]Violation, error) {
	payload := map[string]any{"text": text, "stage": stage}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint+"/v1/validate", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("grayswan: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	var result struct {
		Violations []struct {
			Rule    string `json:"rule"`
			Message string `json:"message"`
			Score   float64 `json:"score"`
		} `json:"violations"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("grayswan: unmarshal: %w", err)
	}

	var violations []Violation
	for _, v := range result.Violations {
		violations = append(violations, Violation{
			RuleID:  "grayswan:" + v.Rule,
			Stage:   Stage(stage),
			Message: v.Message,
			Action:  "block",
		})
	}
	return violations, nil
}

// --- Patronus ---

type PatronusProvider struct {
	endpoint string
	apiKey   string
	client   *http.Client
}

func NewPatronusProvider(cfg ProviderConfig) *PatronusProvider {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &PatronusProvider{
		endpoint: cfg.Endpoint,
		apiKey:   cfg.APIKey,
		client:   &http.Client{Timeout: timeout},
	}
}

func (p *PatronusProvider) Type() ProviderType { return ProviderPatronus }

func (p *PatronusProvider) ValidateInput(ctx context.Context, input string, meta map[string]string) ([]Violation, error) {
	return p.validate(ctx, "input", input)
}

func (p *PatronusProvider) ValidateOutput(ctx context.Context, output string, meta map[string]string) ([]Violation, error) {
	return p.validate(ctx, "output", output)
}

func (p *PatronusProvider) validate(ctx context.Context, stage, text string) ([]Violation, error) {
	payload := map[string]any{
		"text":     text,
		"evaluate": "all",
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint+"/v1/evaluate", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-KEY", p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("patronus: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	var result struct {
		Results []struct {
			Criterion string  `json:"criterion"`
			Passed    bool    `json:"passed"`
			Message   string  `json:"message"`
			Score     float64 `json:"score"`
		} `json:"results"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("patronus: unmarshal: %w", err)
	}

	var violations []Violation
	for _, r := range result.Results {
		if !r.Passed {
			violations = append(violations, Violation{
				RuleID:  "patronus:" + r.Criterion,
				Stage:   Stage(stage),
				Message: r.Message,
				Action:  "block",
			})
		}
	}
	return violations, nil
}

// --- Provider factory ---

// NewProvider creates a Provider from a config.
func NewProvider(cfg ProviderConfig) (Provider, error) {
	switch cfg.Type {
	case ProviderBedrock:
		return NewBedrockProvider(cfg), nil
	case ProviderAzure:
		return NewAzureProvider(cfg), nil
	case ProviderGraySwan:
		return NewGraySwanProvider(cfg), nil
	case ProviderPatronus:
		return NewPatronusProvider(cfg), nil
	default:
		return nil, fmt.Errorf("guardrails: unknown provider type %q", cfg.Type)
	}
}
