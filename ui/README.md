     1|# Koutaku UI
     2|
     3|A modern, production-ready web interface for the [Koutaku AI Gateway](https://github.com/koutaku/koutaku) - providing real-time monitoring, configuration management, and comprehensive observability for your AI infrastructure.
     4|
     5|## Overview
     6|
     7|Koutaku UI is a React + Vite + TanStack Router web dashboard that serves as the control center for your Koutaku AI Gateway. It provides an intuitive interface to monitor AI requests, configure providers, manage MCP clients, and analyze performance metrics.
     8|
     9|### Key Features
    10|
    11|- **Real-time Log Monitoring** - Live streaming dashboard with WebSocket integration
    12|- **Provider Management** - Configure [15+ AI providers](https://github.com/Barkasj/koutaku/tree/main/docs/quickstart/gateway/provider-configuration)
    13|- **MCP Integration** - Manage [Model Context Protocol](https://github.com/Barkasj/koutaku/tree/main/docs/features/mcp) clients for advanced AI capabilities
    14|- **Plugin System** - Extend functionality with [custom plugins](https://github.com/Barkasj/koutaku/tree/main/docs/plugins/getting-started)
    15|- **Analytics Dashboard** - Request metrics, success rates, latency tracking, and token usage
    16|- **Modern UI** - Dark/light mode, responsive design, and accessible components
    17|- **Documentation Hub** - Built-in documentation browser and quick-start guides
    18|
    19|## Quick Start
    20|
    21|### Prerequisites
    22|
    23|The UI is designed to work with the Koutaku HTTP transport backend. Get started with the complete setup:
    24|
    25|**[Gateway Setup Guide →](https://github.com/Barkasj/koutaku/tree/main/docs/quickstart/gateway/setting-up)**
    26|
    27|### Development
    28|
    29|```bash
    30|# Install dependencies
    31|npm install
    32|
    33|# Start development server
    34|npm run dev
    35|```
    36|
    37|The development server runs on `http://localhost:3000` and connects to your Koutaku HTTP transport backend (default: `http://localhost:8080`).
    38|
    39|### Environment Variables
    40|
    41|```bash
    42|# Development only - customize Koutaku backend port
    43|KOUTAKU_PORT=8080
    44|```
    45|
    46|## Architecture
    47|
    48|### Technology Stack
    49|
    50|- **Framework**: React 19 + Vite + TanStack Router
    51|- **Language**: TypeScript
    52|- **Styling**: Tailwind CSS + Radix UI components
    53|- **State Management**: Redux Toolkit with RTK Query
    54|- **Real-time**: WebSocket integration
    55|- **HTTP Client**: Axios with typed service layer
    56|- **Theme**: Dark/light mode support
    57|
    58|### Integration Model
    59|
    60|```
    61|┌─────────────────┐    HTTP/WebSocket    ┌──────────────────┐
    62|│   Koutaku UI    │ ◄─────────────────► │ Koutaku HTTP     │
    63|│   (React+Vite)  │                     │ Transport (Go)   │
    64|└─────────────────┘                     └──────────────────┘
    65|        │                                        │
    66|        │ Build artifacts                        │
    67|        └────────────────────────────────────────┘
    68|```
    69|
    70|- **Development**: UI runs on port 3000, connects to Go backend on port 8080
    71|- **Production**: UI built as static assets served directly by Go HTTP transport
    72|- **Communication**: REST API + WebSocket for real-time features
    73|
    74|## Features
    75|
    76|### Real-time Log Monitoring
    77|
    78|The main dashboard provides comprehensive request monitoring with live updates via WebSocket, advanced filtering, and detailed request/response inspection.
    79|
    80|**[Learn More →](https://github.com/Barkasj/koutaku/tree/main/docs/features/observability)**
    81|
    82|### Provider Configuration
    83|
    84|Manage all your AI providers from a unified interface with support for multiple API keys, custom network configuration, and provider-specific settings.
    85|
    86|**[View All Providers →](https://github.com/Barkasj/koutaku/tree/main/docs/quickstart/gateway/provider-configuration)**
    87|
    88|### MCP Client Management
    89|
    90|Model Context Protocol integration for advanced AI capabilities including tool integration and connection monitoring.
    91|
    92|**[MCP Documentation →](https://github.com/Barkasj/koutaku/tree/main/docs/features/mcp)**
    93|
    94|### Plugin Ecosystem
    95|
    96|Extend Koutaku with powerful plugins for observability, testing, caching, and custom functionality.
    97|
    98|**Available Plugins:**
    99|
   100|- [Koutaku Logger](https://github.com/Barkasj/koutaku/tree/main/docs/features/observability/koutaku) - Advanced LLM observability
   101|- [Response Mocker](https://github.com/Barkasj/koutaku/tree/main/docs/features/plugins/mocker) - Mock responses for testing
   102|- [Semantic Cache](https://github.com/Barkasj/koutaku/tree/main/docs/features/semantic-caching) - Intelligent response caching
   103|- [OpenTelemetry](https://github.com/Barkasj/koutaku/tree/main/docs/features/observability/otel) - Distributed tracing
   104|
   105|**[Plugin Development Guide →](https://github.com/Barkasj/koutaku/tree/main/docs/plugins/getting-started)**
   106|
   107|## Development
   108|
   109|### Project Structure
   110|
   111|```
   112|ui/
   113|├── app/                    # TanStack Router pages
   114|│   ├── page.tsx           # Main logs dashboard
   115|│   ├── config/            # Provider & MCP configuration
   116|│   ├── docs/              # Documentation browser
   117|│   └── plugins/           # Plugin management
   118|├── components/            # Reusable UI components
   119|│   ├── logs/             # Log monitoring components
   120|│   ├── config/           # Configuration forms
   121|│   └── ui/               # Base UI components (Radix)
   122|├── hooks/                # Custom React hooks
   123|├── lib/                  # Utilities and services
   124|│   ├── store/            # Redux store and API slices
   125|│   ├── types/            # TypeScript definitions
   126|│   └── utils/            # Helper functions
   127|└── scripts/              # Build and deployment scripts
   128|```
   129|
   130|### API Integration
   131|
   132|The UI uses Redux Toolkit + RTK Query for state management and API communication with the Koutaku HTTP transport backend:
   133|
   134|```typescript
   135|// Example API usage with RTK Query
   136|import { useGetLogsQuery, useCreateProviderMutation, getErrorMessage } from "@/lib/store";
   137|
   138|// Get real-time logs with automatic caching
   139|const { data: logs, error, isLoading } = useGetLogsQuery({ filters, pagination });
   140|
   141|// Configure provider with optimistic updates
   142|const [createProvider] = useCreateProviderMutation();
   143|
   144|const handleCreate = async () => {
   145|	try {
   146|		await createProvider({
   147|			provider: "openai",
   148|			keys: [{ value: "sk-...", models: ["gpt-4"], weight: 1 }],
   149|			// ... other config
   150|		}).unwrap();
   151|		// Success handling
   152|	} catch (error) {
   153|		console.error(getErrorMessage(error));
   154|	}
   155|};
   156|```
   157|
   158|### Component Guidelines
   159|
   160|- **Composition**: Use Radix UI primitives for accessibility
   161|- **Styling**: Tailwind CSS with CSS variables for theming
   162|- **Types**: Full TypeScript coverage matching Go backend schemas
   163|- **Error Handling**: Consistent error states and user feedback
   164|
   165|### Adding New Features
   166|
   167|1. **Backend Integration**: Add API endpoints to RTK Query slices in `lib/store/`
   168|2. **Type Definitions**: Update types in `lib/types/`
   169|3. **UI Components**: Build with Radix UI and Tailwind
   170|4. **State Management**: Use RTK Query for API state, React hooks for local state
   171|5. **Real-time Updates**: Integrate WebSocket events when applicable
   172|
   173|## Configuration
   174|
   175|### Provider Setup
   176|
   177|The UI supports comprehensive provider configuration including API keys with model assignments, network settings, and provider-specific options.
   178|
   179|**[Complete Provider Configuration Guide →](https://github.com/Barkasj/koutaku/tree/main/docs/quickstart/gateway/provider-configuration)**
   180|
   181|### Governance & Access Control
   182|
   183|Configure virtual keys, budget limits, rate limiting, and team-based access control through the UI.
   184|
   185|**[Governance Documentation →](https://github.com/Barkasj/koutaku/tree/main/docs/features/governance)**
   186|
   187|### Real-time Features
   188|
   189|WebSocket connection provides live log streaming, connection status monitoring, automatic reconnection, and filtered real-time updates.
   190|
   191|**[Observability Features →](https://github.com/Barkasj/koutaku/tree/main/docs/features/observability)**
   192|
   193|## Monitoring & Analytics
   194|
   195|The dashboard provides comprehensive observability including request metrics, token usage tracking, provider performance analysis, error categorization, and historical trend analysis.
   196|
   197|**[Performance Benchmarks →](https://github.com/Barkasj/koutaku/tree/main/docs/benchmarking/getting-started)**
   198|
   199|## Contributing
   200|
   201|We welcome contributions! See our [Contributing Guide](https://github.com/Barkasj/koutaku/tree/main/docs/contributing/setting-up-repo) for:
   202|
   203|- Code conventions and style guide
   204|- Development setup and workflow
   205|- Adding new providers or features
   206|- Plugin development guidelines
   207|
   208|## Documentation
   209|
   210|**Complete Documentation:** [https://github.com/Barkasj/koutaku/tree/main/docs](https://github.com/Barkasj/koutaku/tree/main/docs)
   211|
   212|### Quick Links
   213|
   214|- [Gateway Setup](https://github.com/Barkasj/koutaku/tree/main/docs/quickstart/gateway/setting-up) - Get started in 30 seconds
   215|- [Provider Configuration](https://github.com/Barkasj/koutaku/tree/main/docs/quickstart/gateway/provider-configuration) - Multi-provider setup
   216|- [MCP Integration](https://github.com/Barkasj/koutaku/tree/main/docs/features/mcp) - External tool calling
   217|- [Plugin Development](https://github.com/Barkasj/koutaku/tree/main/docs/plugins/getting-started) - Build custom plugins
   218|- [Architecture](https://github.com/Barkasj/koutaku/tree/main/docs/architecture) - System design and internals
   219|
   220|## Need Help?
   221|
   222|**[Join our Discord](https://discord.gg/exN5KAydbU)** for community support and discussions.
   223|
   224|Get help with:
   225|
   226|- Quick setup assistance and troubleshooting
   227|- Best practices and configuration tips
   228|- Community discussions and support
   229|- Real-time help with integrations
   230|
   231|## Links
   232|
   233|- **Main Repository**: [github.com/Barkasj/koutaku](https://github.com/Barkasj/koutaku)
   234|- **HTTP Transport**: [../transports/koutaku-http](../transports/koutaku-http)
   235|- **Documentation**: [github.com/Barkasj/koutaku/tree/main/docs](https://github.com/Barkasj/koutaku/tree/main/docs)
   236|- **Website**: [github.com/Barkasj/koutaku](https://github.com/Barkasj/koutaku)
   237|
   238|## License
   239|
   240|Licensed under the Apache 2.0 License - see the [LICENSE](../LICENSE) file for details.
   241|
   242|---
   243|
   244|Built with ❤️ by [Koutaku](https://github.com/koutakuhq)