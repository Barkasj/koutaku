/**
 * Test data factories for observability connector tests
 */

/**
 * Observability connector configuration
 */
export interface ObservabilityConnectorConfig {
  type: 'otel' | 'koutaku'
  enabled: boolean
  endpoint?: string
  apiKey?: string
}

/**
 * Create OTEL connector configuration data
 */
export function createOtelConnectorData(overrides: Partial<ObservabilityConnectorConfig> = {}): ObservabilityConnectorConfig {
  return {
    type: 'otel',
    enabled: true,
    endpoint: 'http://localhost:4318',
    ...overrides
  }
}

/**
 * Create Koutaku connector configuration data
 */
export function createKoutakuConnectorData(overrides: Partial<ObservabilityConnectorConfig> = {}): ObservabilityConnectorConfig {
  return {
    type: 'koutaku',
    enabled: true,
    endpoint: 'http://localhost:8080',
    apiKey: 'test-api-key',
    ...overrides
  }
}
