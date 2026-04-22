package guardrails

import (
	"fmt"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/checker/decls"
)

// CELEvaluator compiles and evaluates CEL expressions for guardrail rules.
type CELEvaluator struct {
	env *cel.Env
}

// NewCELEvaluator creates a CEL environment with variables available to
// guardrail rule expressions:
//
//   - input  (string) – the user input text
//   - output (string) – the model output text
//   - model  (string) – the model name
//   - user_id (string) – the authenticated user ID
//   - metadata (map<string, string>) – request metadata
func NewCELEvaluator() (*CELEvaluator, error) {
	env, err := cel.NewEnv(
		cel.Declarations(
			decls.NewVar("input", decls.String),
			decls.NewVar("output", decls.String),
			decls.NewVar("model", decls.String),
			decls.NewVar("user_id", decls.String),
			decls.NewVar("metadata", decls.NewMapType(decls.String, decls.String)),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("guardrails: failed to create CEL env: %w", err)
	}
	return &CELEvaluator{env: env}, nil
}

// EvalContext provides the variable bindings for a single evaluation.
type EvalContext struct {
	Input    string            `json:"input"`
	Output   string            `json:"output"`
	Model    string            `json:"model"`
	UserID   string            `json:"user_id"`
	Metadata map[string]string `json:"metadata"`
}

// Evaluate compiles and runs a CEL expression against the given context.
// Returns (result, nil) on success. The result is a boolean indicating
// whether the rule condition is triggered (true = violation detected).
func (e *CELEvaluator) Evaluate(expression string, ctx EvalContext) (bool, error) {
	ast, issues := e.env.Parse(expression)
	if issues != nil && issues.Err() != nil {
		return false, fmt.Errorf("guardrails: parse error in %q: %w", expression, issues.Err())
	}
	checked, issues := e.env.Check(ast)
	if issues != nil && issues.Err() != nil {
		return false, fmt.Errorf("guardrails: check error in %q: %w", expression, issues.Err())
	}
	prog, err := e.env.Program(checked)
	if err != nil {
		return false, fmt.Errorf("guardrails: program error in %q: %w", expression, err)
	}

	vars := map[string]any{
		"input":    ctx.Input,
		"output":   ctx.Output,
		"model":    ctx.Model,
		"user_id":  ctx.UserID,
		"metadata": ctx.Metadata,
	}

	val, _, err := prog.Eval(vars)
	if err != nil {
		return false, fmt.Errorf("guardrails: eval error in %q: %w", expression, err)
	}

	b, ok := val.Value().(bool)
	if !ok {
		return false, fmt.Errorf("guardrails: expression %q returned %T, expected bool", expression, val.Value())
	}
	return b, nil
}

// Validate checks an expression compiles without running it.
func (e *CELEvaluator) Validate(expression string) error {
	ast, issues := e.env.Parse(expression)
	if issues != nil && issues.Err() != nil {
		return fmt.Errorf("guardrails: parse error: %w", issues.Err())
	}
	_, issues = e.env.Check(ast)
	if issues != nil && issues.Err() != nil {
		return fmt.Errorf("guardrails: check error: %w", issues.Err())
	}
	return nil
}
