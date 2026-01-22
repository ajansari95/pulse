# 💓 Pulse

Lightweight endpoint monitoring with Telegram alerts. Single binary, zero dependencies.

```
pulse config.yaml
```

## Features

- 🔍 **Multi-protocol** - HTTP, JSON-RPC, gRPC, Tendermint, TCP ports
- 📱 **Telegram alerts** - Instant down/up/slow notifications
- ⚡ **Response tracking** - Monitor latency trends
- 🔇 **Smart alerting** - Configurable failure threshold reduces noise
- 📝 **YAML config** - Simple, readable configuration
- 📦 **Single binary** - No runtime dependencies

## Install

```bash
go install github.com/ajansari95/pulse@latest
```

Or download from [releases](https://github.com/ajansari95/pulse/releases).

## Quick Start

```bash
# 1. Create config
cat > config.yaml << 'EOF'
telegram:
  bot_token: ""
  chat_id: ""

settings:
  check_interval: "60s"
  failure_threshold: 2
  slow_threshold: "5s"

endpoints:
  - name: "API"
    url: "https://api.example.com/health"
    type: "http"
EOF

# 2. Set credentials (or add to config)
export TELEGRAM_BOT_TOKEN="your-token"
export TELEGRAM_CHAT_ID="your-chat-id"

# 3. Run
pulse config.yaml
```

## Configuration

```yaml
telegram:
  bot_token: ""    # or TELEGRAM_BOT_TOKEN env
  chat_id: ""      # or TELEGRAM_CHAT_ID env

settings:
  check_interval: "60s"      # how often to check
  failure_threshold: 2       # alert after N consecutive failures
  slow_threshold: "5s"       # alert if response exceeds this
  port: "8080"               # health endpoint port

endpoints:
  - name: "My API"
    url: "https://api.example.com/health"
    type: "http"
    expected: 200

  - name: "Ethereum RPC"
    url: "https://eth.example.com"
    type: "jsonrpc"
    method: "eth_blockNumber"

  - name: "Cosmos RPC"
    url: "https://rpc.cosmos.example.com"
    type: "tendermint"

  - name: "gRPC Service"
    url: "grpc.example.com:443"
    type: "grpc"

  - name: "Database"
    url: "db.example.com"
    type: "port"
    port: 5432
```

## Endpoint Types

| Type | Description | Config |
|------|-------------|--------|
| `http` | HTTP GET | `url`, `expected` (default: 200) |
| `jsonrpc` | JSON-RPC POST | `url`, `method` (default: eth_blockNumber) |
| `tendermint` | Tendermint /status | `url` |
| `grpc` | gRPC health check | `url` (host:port) |
| `port` | TCP connect | `url` (host), `port` |

## Telegram Setup

1. Message [@BotFather](https://t.me/BotFather) → `/newbot`
2. Copy the token
3. Add bot to your channel/group as admin
4. Get chat ID: forward message to [@userinfobot](https://t.me/userinfobot)

### Commands

Send `/status` in chat to get current status of all endpoints.

## Deployment

### Systemd

```bash
sudo tee /etc/systemd/system/pulse.service << EOF
[Unit]
Description=Pulse Monitor
After=network.target

[Service]
Type=simple
Environment=TELEGRAM_BOT_TOKEN=xxx
Environment=TELEGRAM_CHAT_ID=xxx
ExecStart=/usr/local/bin/pulse /etc/pulse/config.yaml
Restart=always

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl enable --now pulse
```

### Docker

```bash
docker run -d \
  -e TELEGRAM_BOT_TOKEN="xxx" \
  -e TELEGRAM_CHAT_ID="xxx" \
  -v $(pwd)/config.yaml:/config.yaml \
  ghcr.io/ajansari95/pulse
```

## Health API

```bash
curl http://localhost:8080/health
```

```json
{
  "API": {"up": true, "response_time_ms": 120, "last_check": "..."},
  "Database": {"up": false, "last_error": "connection refused", "last_check": "..."}
}
```

## License

MIT
