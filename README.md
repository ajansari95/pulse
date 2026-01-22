# Pulse

Lightweight endpoint monitoring with Telegram alerts. Single binary, zero dependencies.

## Features

- **Multi-protocol** - HTTP, JSON-RPC, gRPC, Tendermint, TCP, WebSocket
- **Flexible HTTP** - Any method, custom headers, request body, auth
- **Response validation** - Status codes, body contains, regex, JSON path
- **Authentication** - Basic auth, Bearer token
- **TLS options** - Skip verification for self-signed certs
- **Per-endpoint config** - Custom timeout, headers per endpoint
- **Environment variables** - Use `${VAR}` syntax in config
- **Telegram alerts** - Instant down/up/slow notifications
- **Summary reports** - Daily/weekly uptime summaries
- **Webhook notifications** - Slack, Discord, PagerDuty
- **Web dashboard** - Status, charts, incident history
- **Prometheus metrics** - `/metrics` endpoint for scraping
- **Maintenance windows** - Scheduled/acknowledged incident suppression
- **Single binary** - No runtime dependencies

## Install

```bash
go install github.com/ajansari95/pulse@latest
```

## Quick Start

```bash
# Create config
cat > config.yaml << 'EOF'
telegram:
  bot_token: ""
  chat_id: ""

notifications:
  slack:
    webhook_url: "https://hooks.slack.com/..."
  discord:
    webhook_url: "https://discord.com/api/webhooks/..."
  pagerduty:
    routing_key: "your-integration-key"

settings:
  check_interval: "60s"
  failure_threshold: 2

endpoints:
  - name: "My API"
    url: "https://api.example.com/health"
    type: "http"
EOF

# Set credentials
export TELEGRAM_BOT_TOKEN="your-token"
export TELEGRAM_CHAT_ID="your-chat-id"

# Run
pulse config.yaml
```

## Configuration

### Settings

```yaml
settings:
  check_interval: "60s"      # How often to check
  failure_threshold: 2       # Alert after N consecutive failures
  slow_threshold: "5s"       # Alert if response exceeds this
  timeout: "10s"             # Default request timeout
  port: "8080"               # Health endpoint port
  metrics_port: "9090"       # Metrics endpoint port (optional)
  daily_summary: "09:00"     # Daily summary time (local)
  weekly_summary: "monday 09:00" # Weekly summary schedule (local)
  public_dashboard: true      # Enable the web dashboard
```

### Maintenance Windows

```yaml
maintenance:
  - name: "Weekly deploy"
    schedule: "sunday 02:00-04:00"
    endpoints: ["API", "Database"]
```

### HTTP Endpoints

```yaml
endpoints:
  # Simple GET
  - name: "API"
    url: "https://api.example.com/health"
    type: "http"

  # POST with body and headers
  - name: "API POST"
    url: "https://api.example.com/check"
    type: "http"
    method: "POST"
    content_type: "application/json"
    body: '{"check": true}'
    headers:
      X-Custom: "value"

  # Response validation
  - name: "API Validated"
    url: "https://api.example.com/status"
    type: "http"
    expected: 200
    contains: "healthy"
    json_path: "status"
    json_path_value: "ok"

  # Multiple valid status codes
  - name: "Flexible API"
    url: "https://api.example.com/endpoint"
    type: "http"
    expected_codes: [200, 201, 204]

  # Regex match
  - name: "Version"
    url: "https://api.example.com/version"
    type: "http"
    match_regex: "v[0-9]+\\.[0-9]+"
```

### Authentication

```yaml
endpoints:
  # Bearer token
  - name: "Protected API"
    url: "https://api.example.com/admin"
    type: "http"
    bearer_token: "${API_TOKEN}"

  # Basic auth
  - name: "Admin API"
    url: "https://internal.example.com/admin"
    type: "http"
    basic_auth:
      username: "${ADMIN_USER}"
      password: "${ADMIN_PASS}"
```

### TLS Options

```yaml
endpoints:
  - name: "Internal"
    url: "https://internal.local:8443/health"
    type: "http"
    skip_tls_verify: true
    timeout: "5s"
```

### Other Protocols

```yaml
endpoints:
  # JSON-RPC (Ethereum, etc.)
  - name: "Ethereum"
    url: "https://eth.example.com"
    type: "jsonrpc"
    rpc_method: "eth_blockNumber"

  # Tendermint/Cosmos
  - name: "Cosmos"
    url: "https://rpc.cosmos.example.com"
    type: "tendermint"

  # gRPC
  - name: "gRPC Service"
    url: "grpc.example.com:443"
    type: "grpc"

  # TCP port
  - name: "Database"
    url: "db.example.com"
    type: "port"
    port: 5432

  # TCP direct
  - name: "Redis"
    url: "redis.example.com:6379"
    type: "tcp"

  # WebSocket
  - name: "WS API"
    url: "wss://ws.example.com/stream"
    type: "websocket"
```

## Environment Variables

Use `${VAR}` in config - they expand at load time:

```yaml
endpoints:
  - name: "API"
    url: "${API_URL}/health"
    bearer_token: "${API_TOKEN}"
```

Override settings via env:
- `TELEGRAM_BOT_TOKEN`
- `TELEGRAM_CHAT_ID`
- `CHECK_INTERVAL`
- `PORT`
- `METRICS_PORT`
- `PUBLIC_DASHBOARD`

## Prometheus Metrics

Expose metrics for scraping at `GET /metrics`. If `metrics_port` is set, metrics are served on that port; otherwise, they are served on the main HTTP port.

## Web Dashboard

The dashboard is served at `/` and `/dashboard` when enabled. It provides real-time status, response-time charts, and incident history.

API endpoints:
- `/api/summary` for status and aggregate metrics
- `/api/history` for response history and incidents

## Webhook Notifications

Pulse can also send alerts to Slack, Discord, and PagerDuty. Configure the webhook URLs or routing key under `notifications` in your config.

## SSL Certificate Monitoring

Monitor certificate expiry with a dedicated `ssl` endpoint type:

```yaml
endpoints:
  - name: "API SSL"
    url: "https://api.example.com"
    type: "ssl"
    warn_days: 30
    critical_days: 7
```

## Endpoint Types

| Type | Check Method |
|------|--------------|
| `http` | HTTP request with configurable method/headers/body |
| `jsonrpc` | JSON-RPC 2.0 POST |
| `tendermint` | Tendermint /status endpoint |
| `grpc` | gRPC health check |
| `port` / `tcp` | TCP connection |
| `websocket` | TCP connection to WS endpoint |
| `ssl` | TLS certificate expiry check |

## Telegram Setup

1. Message [@BotFather](https://t.me/BotFather) → `/newbot`
2. Copy the token
3. Add bot to your channel/group as admin
4. Get chat ID: forward a message to [@userinfobot](https://t.me/userinfobot)

## Telegram Commands

- `/status` - Current status summary
- `/mute <endpoint> <duration>` - Mute alerts (example: `/mute API 30m`)
- `/ack <endpoint>` - Acknowledge an incident
- `/maintenance start <endpoint> <duration>` - Start a maintenance window (example: `/maintenance start API 2h`)

Send `/status` in chat to get current status.

## Health API

```bash
curl http://localhost:8080/health
```

Returns JSON with all endpoint statuses. Returns 503 if any endpoint is down.

## Docker

```bash
docker run -d \
  -e TELEGRAM_BOT_TOKEN="xxx" \
  -e TELEGRAM_CHAT_ID="xxx" \
  -v $(pwd)/config.yaml:/config.yaml \
  ghcr.io/ajansari95/pulse
```

## License

MIT
