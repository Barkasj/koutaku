// Package llmtests provides comprehensive test utilities and configurations for the Koutaku system.
// It includes comprehensive test implementations covering all major AI provider scenarios,
// including text completion, chat, tool calling, image processing, and end-to-end workflows.
package llmtests

import (
	"context"
	"time"

	koutaku "github.com/koutaku/koutaku/core"
	"github.com/koutaku/koutaku/core/schemas"
)

// Constants for test configuration
const (
	// TestTimeout defines the maximum duration for comprehensive tests
	// Set to 20 minutes to allow for complex multi-step operations
	TestTimeout = 20 * time.Minute
)

// getKoutaku initializes and returns a Koutaku instance for comprehensive testing.
// It sets up the comprehensive test account, plugin, and logger configuration.
//
// Environment variables are expected to be set by the system or test runner before calling this function.
// The account configuration will read API keys and settings from these environment variables.
//
// Returns:
//   - *koutaku.Koutaku: A configured Koutaku instance ready for comprehensive testing
//   - error: Any error that occurred during Koutaku initialization
//
// The function:
//  1. Creates a comprehensive test account instance
//  2. Configures Koutaku with the account and default logger
func getKoutaku(ctx context.Context) (*koutaku.Koutaku, error) {
	account := ComprehensiveTestAccount{}

	// Initialize Koutaku
	b, err := koutaku.Init(ctx, schemas.KoutakuConfig{
		Account: &account,
		Logger:  koutaku.NewDefaultLogger(schemas.LogLevelDebug),
	})
	if err != nil {
		return nil, err
	}

	return b, nil
}

// SetupTest initializes a test environment with timeout context
func SetupTest() (*koutaku.Koutaku, context.Context, context.CancelFunc, error) {
	ctx, cancel := context.WithTimeout(context.Background(), TestTimeout)
	client, err := getKoutaku(ctx)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}

	return client, ctx, cancel, nil
}
