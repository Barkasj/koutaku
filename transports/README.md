# Koutaku Gateway

Koutaku Gateway is a blazing-fast HTTP API that unifies access to 15+ AI providers (OpenAI, Anthropic, AWS Bedrock, Google Vertex, and more) through a single OpenAI-compatible interface. Deploy in seconds with zero configuration and get automatic fallbacks, semantic caching, tool calling, and enterprise-grade features.

**Complete Documentation**: [https://github.com/Barkasj/koutaku/tree/main/docs](https://github.com/Barkasj/koutaku/tree/main/docs)

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

- **[Unified Interface](https://github.com/Barkasj/koutaku/tree/main/docs/features/unified-interface)** - Single OpenAI-compatible API for all providers
- **[Multi-Provider Support](https://github.com/Barkasj/koutaku/tree/main/docs/quickstart/gateway/provider-configuration)** - OpenAI, Anthropic, AWS Bedrock, Google Vertex, Cerebras, Azure, Cohere, Mistral, Ollama, Groq, and more
- **[Drop-in Replacement](https://github.com/Barkasj/koutaku/tree/main/docs/features/drop-in-replacement)** - Replace OpenAI/Anthropic/GenAI SDKs with zero code changes
- **[Automatic Fallbacks](https://github.com/Barkasj/koutaku/tree/main/docs/features/fallbacks)** - Seamless failover between providers and models
- **[Streaming Support](https://github.com/Barkasj/koutaku/tree/main/docs/quickstart/gateway/streaming)** - Real-time response streaming for all providers

### Advanced Features

- **[Model Context Protocol (MCP)](https://github.com/Barkasj/koutaku/tree/main/docs/features/mcp)** - Enable AI models to use external tools (filesystem, web search, databases)
- **[Semantic Caching](https://github.com/Barkasj/koutaku/tree/main/docs/features/semantic-caching)** - Intelligent response caching based on semantic similarity
- **[Load Balancing](https://github.com/Barkasj/koutaku/tree/main/docs/features/fallbacks)** - Distribute requests across multiple API keys and providers
- **[Governance & Budget Management](https://github.com/Barkasj/koutaku/tree/main/docs/features/governance)** - Usage tracking, rate limiting, and cost control
- **[Custom Plugins](https://github.com/Barkasj/koutaku/tree/main/docs/enterprise/custom-plugins)** - Extensible middleware for analytics, monitoring, and custom logic

### Enterprise Features

- **[Clustering](https://github.com/Barkasj/koutaku/tree/main/docs/enterprise/clustering)** - Multi-node deployment with shared state
- **[SSO Integration](https://github.com/Barkasj/koutaku/tree/main/docs/features/sso-with-google-github)** - Google, GitHub authentication
- **[Vault Support](https://github.com/Barkasj/koutaku/tree/main/docs/enterprise/vault-support)** - Secure API key management
- **[Custom Analytics](https://github.com/Barkasj/koutaku/tree/main/docs/features/observability)** - Detailed usage insights and monitoring
- **[In-VPC Deployments](https://github.com/Barkasj/koutaku/tree/main/docs/enterprise/invpc-deployments)** - Private cloud deployment options

**Learn More**: [Complete Feature Documentation](https://github.com/Barkasj/koutaku/tree/main/docs/features/unified-interface)

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

**Complete Integration Guides**: [SDK Integrations](https://github.com/Barkasj/koutaku/tree/main/docs/integrations/what-is-an-integration)

---

## Documentation

### Getting Started

- [Quick Setup Guide](https://github.com/Barkasj/koutaku/tree/main/docs/quickstart/gateway/setting-up) - Detailed installation and configuration
- [Provider Configuration](https://github.com/Barkasj/koutaku/tree/main/docs/quickstart/gateway/provider-configuration) - Connect multiple AI providers
- [Integration Guide](https://github.com/Barkasj/koutaku/tree/main/docs/quickstart/gateway/integrations) - SDK replacements

### Advanced Topics

- [MCP Tool Calling](https://github.com/Barkasj/koutaku/tree/main/docs/features/mcp) - External tool integration
- [Semantic Caching](https://github.com/Barkasj/koutaku/tree/main/docs/features/semantic-caching) - Intelligent response caching
- [Fallbacks & Load Balancing](https://github.com/Barkasj/koutaku/tree/main/docs/features/fallbacks) - Reliability and scaling
- [Budget Management](https://github.com/Barkasj/koutaku/tree/main/docs/features/governance) - Cost control and governance

**Browse All Documentation**: [https://github.com/Barkasj/koutaku/tree/main/docs](https://github.com/Barkasj/koutaku/tree/main/docs)

---

*Built with ❤️ by [Koutaku](https://github.com/Barkasj/koutaku)*
