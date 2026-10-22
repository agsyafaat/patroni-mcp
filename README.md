# Patroni MCP server (Go) — reference implementation

A small, dependency-free MCP **stdio** server for Patroni REST API. Designed for local Claude Desktop / Cursor execution or over SSH with stdio forwarding. This is a **reference implementation**, not an audited production release.

## Features

- MCP JSON-RPC `initialize`, `ping`, `tools/list`, `tools/call` (protocol 2024-11-05).
- Read-only: `patroni_cluster_status`, `patroni_node_status`, `patroni_get_config`, `patroni_replication_status`.
- Optional writes: `patroni_switchover`, `patroni_failover`, `patroni_update_config`.
- Cluster allowlist; HTTPS with optional custom CA and client certificate; optional basic authentication via environment variable; timeouts; redirects disabled; bounded response size; stderr audit logs.

## Build

Requires Go 1.22+.

```bash
go build -trimpath -o patroni-mcp ./cmd/patroni-mcp
GOOS=windows GOARCH=amd64 go build -trimpath -o patroni-mcp.exe ./cmd/patroni-mcp
```

## Configure

Copy `configs/config.example.json` to a protected location and edit the cluster URL, TLS CA and authentication. Set `PATRONI_API_PASSWORD` in the MCP process environment. Never commit secrets. Prefer HTTPS and a VPN/private management network. `username` / `password_env` can be omitted if your Patroni read endpoints don't require authentication. Patroni REST API authentication/authorization varies by deployment; test actual permissions.

**Important:** A cluster alias points to **one Patroni node URL**. If that node becomes unavailable, the server does not discover another automatically. Use a stable management endpoint or enhance the implementation with multiple endpoints and read-only fallback.

## Claude Desktop (Windows)

Build `patroni-mcp.exe` for Windows and put it in `C:\Tools`. Add the `mcpServers.patroni` object from `examples/claude_desktop_config.json` into your existing Claude Desktop config; **do not replace the rest of your existing JSON**. Change the config path to the actual Windows path, and ensure the TLS CA file path is also a Windows path. Restart Claude Desktop. For security, avoid putting real credentials in the JSON: use a process environment or secure launcher instead.

## Linux CLI smoke test

```bash
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"smoke","version":"1"}}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"patroni_cluster_status","arguments":{"cluster":"production"}}}' \
  | ./patroni-mcp --config configs/config.example.json
```

The server only prints protocol messages to stdout. Errors and audit logs go to stderr.

## Linux deployment / systemd

**Stdio MCP servers should normally be spawned by their MCP client.** A normal always-running systemd service has no MCP client attached and is **not a usable network MCP endpoint**. `deploy/patroni-mcp.service` is an illustrative hardening template only and intentionally not an immediately usable service. For a remote Linux host, run this binary over an SSH command from your MCP client, or deploy a separately implemented HTTP MCP gateway with authentication and proper session management. Do **not** expose Patroni REST directly to untrusted clients.

## Write operation controls

- `allow_writes=true` enables switchover.
- `allow_failover=true` additionally enables failover.
- `allow_config_writes=true` additionally enables restricted config PATCH.
- Only `loop_wait`, `retry_timeout`, `maximum_lag_on_failover`, and `ttl` are accepted for config updates, as nonnegative integers. Cross-field Patroni constraints are **not** checked; review and test values manually.
- **No server-side approval workflow is implemented.** Client-side confirmation alone is insufficient for unattended/automated production access. Keep all write flags `false` in production unless you add a separate human-approval and policy layer.
- The service does not preflight replication health or leader identity before write requests. Patroni performs its own checks; use the read-only tools and operator review first.

## Limitations / hardening before production

- Minimal hand-written MCP transport (no full MCP SDK). Protocol interoperability tested only with a local smoke test, not Claude Desktop.
- Reads and writes target the configured endpoint, not necessarily the leader. A cluster failover may require endpoint changes.
- The `patroni_replication_status` tool returns Patroni `/cluster` members and lag, not detailed `pg_stat_replication` data.
- Writes only log operation name and cluster alias; no durable tamper-resistant audit store, user identity, or approval.
- No rate limiting, retries, metrics, tracing, or HA endpoint discovery.
- Configuration responses can contain operationally sensitive information. Restrict access.
- Only use HTTPS to avoid exposing basic-auth credentials; this implementation does not enforce HTTPS.

Official Patroni API reference: https://patroni.readthedocs.io/en/latest/rest_api.html
