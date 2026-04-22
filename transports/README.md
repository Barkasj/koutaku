# Koutaku Gateway

Koutaku Gateway is a blazing-fast HTTP API that unifies access to 15+ AI providers (OpenAI, Anthropic, AWS Bedrock, Google Vertex, and more) through a single OpenAI-compatible interface. Deploy in seconds with zero configuration and get automatic fallbacks, semantic caching, tool calling, and enterprise-grade features.

**Complete Documentation**: [https://docs.getkoutaku.ai](https://docs.getkoutaku.ai)

---

## Quick Start

### Installation

Choose your preferred method:

#### NPX (Recommended)

```bash
# Install and run locally
npx -y @koutaku/koutaku

# Open web interface at http://localhost:8080
```

#### Docker

```bash
# Pull and run Koutaku Gateway
docker pull koutaku/koutaku
docker run -p 8080:8080 koutaku/koutaku

# For persistent configuration
docker run -p 8080:8080 -v $(pwd)/data:/app/data koutaku/koutaku
```

### Configuration

Koutaku starts with zero configuration needed. Configure providers through the **built-in web UI** at `http://localhost:8080` or via API:

```bash
# Add OpenAI provider via API
curl -X POST http://localhost:8080/api/providers \
  -H "Content-Type: application/json" \
  -d '{
    "provider": "openai",
    "keys": [{"value": "sk-your-openai-key", "models": ["gpt-4o-mini"], "weight": 1.0}]
  }'
```

For file-based configuration, create `config.json` in your app directory:

```json
{
  "providers": {
    "openai": {
      "keys": [{"value": "env.OPENAI_API_KEY", "models": ["gpt-4o-mini"], "weight": 1.0}]
    }
  }
}
```

### Your First API Call

```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "openai/gpt-4o-mini",
    "messages": [{"role": "user", "content": "Hello, Koutaku!"}]
  }'
```

**That's it!** You now have a unified AI gateway running locally.

---

## Key Features

Koutaku Gateway provides enterprise-grade AI infrastructure with these core capabilities:

### Core Features

- **[Unified Interface](https://docs.getkoutaku.ai/features/unified-interface)** - Single OpenAI-compatible API for all providers
- **[Multi-Provider Support](https://docs.getkoutaku.ai/quickstart/gateway/provider-configuration)** - OpenAI, Anthropic, AWS Bedrock, Google Vertex, Cerebras, Azure, Cohere, Mistral, Ollama, Groq, and more
- **[Drop-in Replacement](https://docs.getkoutaku.ai/features/drop-in-replacement)** - Replace OpenAI/Anthropic/GenAI SDKs with zero code changes
- **[Automatic Fallbacks](https://docs.getkoutaku.ai/features/fallbacks)** - Seamless failover between providers and models
- **[Streaming Support](https://docs.getkoutaku.ai/quickstart/gateway/streaming)** - Real-time response streaming for all providers

### Advanced Features

- **[Model Context Protocol (MCP)](https://docs.getkoutaku.ai/features/mcp)** - Enable AI models to use external tools (filesystem, web search, databases)
- **[Semantic Caching](https://docs.getkoutaku.ai/features/semantic-caching)** - Intelligent response caching based on semantic similarity
- **[Load Balancing](https://docs.getkoutaku.ai/features/fallbacks)** - Distribute requests across multiple API keys and providers
- **[Governance & Budget Management](https://docs.getkoutaku.ai/features/governance)** - Usage tracking, rate limiting, and cost control
- **[Custom Plugins](https://docs.getkoutaku.ai/enterprise/custom-plugins)** - Extensible middleware for analytics, monitoring, and custom logic

### Enterprise Features

- **[Clustering](https://docs.getkoutaku.ai/enterprise/clustering)** - Multi-node deployment with shared state
- **[SSO Integration](https://docs.getkoutaku.ai/features/sso-with-google-github)** - Google, GitHub authentication
- **[Vault Support](https://docs.getkoutaku.ai/enterprise/vault-support)** - Secure API key management
- **[Custom Analytics](https://docs.getkoutaku.ai/features/observability)** - Detailed usage insights and monitoring
- **[In-VPC Deployments](https://docs.getkoutaku.ai/enterprise/invpc-deployments)** - Private cloud deployment options

**Learn More**: [Complete Feature Documentation](https://docs.getkoutaku.ai/features/unified-interface)

---

## SDK Integrations

Replace your existing SDK base URLs to unlock Koutaku's features instantly:

### OpenAI SDK

```python
import openai
client = openai.OpenAI(
    base_url="http://localhost:8080/openai",
    api_key="dummy"  # Handled by Koutaku
)
```

### Anthropic SDK

```python
import anthropic
client = anthropic.Anthropic(
    base_url="http://localhost:8080/anthropic",
    api_key="dummy"  # Handled by Koutaku
)
```

### Google GenAI SDK

```python
import google.generativeai as genai
genai.configure(
    transport="rest",
    api_endpoint="http://localhost:8080/genai",
    api_key="dummy"  # Handled by Koutaku
)
```

**Complete Integration Guides**: [SDK Integrations](https://docs.getkoutaku.ai/integrations/what-is-an-integration)

---

## Documentation

### Getting Started

- [Quick Setup Guide](https://docs.getkoutaku.ai/quickstart/gateway/setting-up) - Detailed installation and configuration
- [Provider Configuration](https://docs.getkoutaku.ai/quickstart/gateway/provider-configuration) - Connect multiple AI providers
- [Integration Guide](https://docs.getkoutaku.ai/quickstart/gateway/integrations) - SDK replacements

### Advanced Topics

- [MCP Tool Calling](https://docs.getkoutaku.ai/features/mcp) - External tool integration
- [Semantic Caching](https://docs.getkoutaku.ai/features/semantic-caching) - Intelligent response caching
- [Fallbacks & Load Balancing](https://docs.getkoutaku.ai/features/fallbacks) - Reliability and scaling
- [Budget Management](https://docs.getkoutaku.ai/features/governance) - Cost control and governance

**Browse All Documentation**: [https://docs.getkoutaku.ai](https://docs.getkoutaku.ai)

---

*Built with ❤️ by [Koutaku](https://getkoutaku.ai)*
