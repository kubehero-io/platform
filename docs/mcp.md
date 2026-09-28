# KubeHero MCP server

`kubehero mcp` serves KubeHero to AI assistants — Claude Code, Claude
Desktop, or any [Model Context Protocol](https://modelcontextprotocol.io)
host — as a set of **read-only** tools. An assistant can look at cost,
logs, profiles, the network map, alerts and rightsizing, and can ask
KubeHero's own investigation agent a question. It cannot change anything:
proposals come back as policy manifests (BudgetPolicy / CeilingPolicy /
RightsizingPolicy) that a human applies through KubeHero's arming flow.

## Setup

The server uses the CLI's configuration — endpoint and bearer token — so
log in once:

```bash
kubehero auth login --endpoint https://kubehero.example.com --token "$KUBEHERO_TOKEN"
kubehero auth whoami          # check who the token authenticates as
```

or pass `KUBEHERO_ENDPOINT` / `KUBEHERO_TOKEN` in the host's environment.
If the advisor service is exposed separately from the control plane, set
`KUBEHERO_ADVISOR_ENDPOINT` (or `--advisor-endpoint`) so `investigate` and
`get_briefing` reach it.

### Claude Code

```bash
claude mcp add kubehero -- kubehero mcp
```

With an explicit endpoint and token:

```bash
claude mcp add kubehero \
  -e KUBEHERO_ENDPOINT=https://kubehero.example.com \
  -e KUBEHERO_TOKEN="$KUBEHERO_TOKEN" \
  -- kubehero mcp
```

### Claude Desktop

Add to `claude_desktop_config.json` (Settings → Developer → Edit Config):

```json
{
  "mcpServers": {
    "kubehero": {
      "command": "kubehero",
      "args": ["mcp"],
      "env": {
        "KUBEHERO_ENDPOINT": "https://kubehero.example.com",
        "KUBEHERO_TOKEN": "<token>"
      }
    }
  }
}
```

Use the absolute path to the binary (`which kubehero`) if Claude Desktop
does not inherit your shell's `PATH`.

### Streamable HTTP

For hosts that connect over HTTP, run the server yourself:

```bash
kubehero mcp --http :8765
claude mcp add --transport http kubehero http://127.0.0.1:8765/
```

A bare port binds **127.0.0.1 only**. Passing an explicit host
(`--http 0.0.0.0:8765`) exposes the server — and with it, read access to
your fleet under your token — to anyone who can reach the port; the
server warns when it listens beyond loopback.

## Tools

| Tool | What it returns | Backing RPC |
|---|---|---|
| `list_clusters` | Registered clusters | `ControlPlaneService.ListClusters` |
| `get_cost_allocation` | Cost by up to 3 dimensions with CPU/RAM/GPU/network/idle breakdown, efficiency, recoverable | `CostService.GetAllocation` |
| `get_cost_timeseries` | Spend per step, grouped, plus a month forecast | `CostService.GetCostTimeseries` |
| `list_rightsizing` | Per-container current vs recommended requests, savings, confidence, OOM kills | `CostService.ListRightsizing` |
| `query_logs` | LogQL lines (≤100, bodies truncated) or metric series | `LogsService.QueryLogs` |
| `get_log_patterns` | Log templates with counts and share | `LogsService.GetLogPatterns` |
| `get_top_functions` | Profiling top functions with $/mo | `ProfilesService.GetTopFunctions` |
| `get_service_map` | eBPF service-map edges by $/mo, cross-zone/egress, retransmits | `NetworkService.GetServiceMap` |
| `list_network_costs` | Per-workload egress + cross-zone spend | `NetworkService.ListNetworkCosts` |
| `list_alerts` | Alerts with state, severity, labels | `AlertsService.ListAlerts` |
| `list_anomalies` | Anomalous spend/capacity/posture signals by impact | `ControlPlaneService.ListAnomalies` |
| `get_briefing` | The advisor's briefing and guarded proposals | `AdvisorService.GetBriefing` |
| `investigate` | An investigation: answer, cited evidence, steps, proposals | `AdvisorService.Investigate` |

Every tool is annotated `readOnlyHint: true`. Inputs are validated
(enums, lengths, LogQL shape, namespace/service name syntax) before any
call; invalid input comes back as a tool error the model can correct.
Results are compact JSON capped at 16 KB so a single call can't flood the
model's context — narrow the query (filters, `limit`, a shorter window)
when a result says it was truncated.

## Example prompts

- "Which namespaces cost the most this week, and how efficient are they?"
- "Show me the top error patterns in payments over the last hour."
- "What's the hottest function in checkout-api, and what does it cost?"
- "Investigate why checkout's spend jumped last night."

## Security notes

- The server holds your token and runs with its RBAC role; give it a
  `viewer` token when the assistant only needs to read.
- stdio mode speaks the protocol on stdout; logs and warnings go to
  stderr.
- Nothing the assistant does can mutate the cluster or KubeHero state:
  the tool set has no write operations.
