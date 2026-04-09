![Image](https://github.com/user-attachments/assets/89be446b-9b88-464a-bb4b-52a005969eea)

<p align="center">
    <a href="./LICENSE"><img src="https://img.shields.io/badge/license-MIT-_red.svg"></a>
    <a href="https://qlty.sh/gh/JoshuaMart/projects/Hookd"><img src="https://qlty.sh/badges/34ecedd0-170b-4fa5-8388-663432d25c6f/maintainability.svg" /></a>
    <a href="https://qlty.sh/gh/JoshuaMart/projects/Hookd"><img src="https://qlty.sh/badges/34ecedd0-170b-4fa5-8388-663432d25c6f/coverage.svg" /></a>
    <img src="https://img.shields.io/badge/golang-1.26-blue?logo=go">
</p>

Lightweight interaction server for capturing out-of-band DNS and HTTP callbacks. Register ephemeral hooks, inject their endpoints into targets, and poll back to check for interactions — ideal for security testing, webhook debugging, and external service monitoring.

## Pipeline

```
1. Register — POST /register creates a hook with unique DNS + HTTP(S) endpoints
        |
        v
2. Inject — use the hook endpoints in your payloads, webhooks, or test targets
        |
        v
3. Capture — Hookd records every DNS query and HTTP request hitting the hook
        |  ┌─────────────────────────────────────────────────────┐
        |  │  DNS server (UDP 53)  → captures qname, qtype, IP  │
        |  │  HTTP/HTTPS (80/443)  → captures method, path,     │
        |  │                          headers, body, IP          │
        |  └─────────────────────────────────────────────────────┘
        v
4. Poll — GET /poll/{id} retrieves captured interactions (and clears them)
        |
        v
5. Evict — background cleanup: TTL expiry, per-hook limits, memory pressure
```

## Quick start

### Binary

Download from [latest build artifacts](https://github.com/JoshuaMart/Hookd/actions/workflows/build.yml).

```bash
chmod +x hookd-linux-amd64
sudo mv hookd-linux-amd64 /usr/local/bin/hookd
sudo mkdir -p /etc/hookd /var/lib/hookd/certs
sudo cp server/config.example.yaml /etc/hookd/config.yaml

sudo hookd --config /etc/hookd/config.yaml
```

<details>
<summary>Available binaries</summary>

| Binary | Platform |
|---|---|
| `hookd-linux-amd64` | Linux x86_64 |
| `hookd-linux-arm64` | Linux ARM64 |
| `hookd-darwin-amd64` | macOS Intel |
| `hookd-darwin-arm64` | macOS Apple Silicon |

</details>

### Usage

```bash
# Register a hook
curl -X POST https://hookd.example.com/register \
  -H "X-API-Key: YOUR_TOKEN"

# Poll interactions
curl https://hookd.example.com/poll/HOOK_ID \
  -H "X-API-Key: YOUR_TOKEN"
```

## Configuration

See [`server/config.example.yaml`](./server/config.example.yaml) for all options.

```yaml
server:
  domain: "hookd.domain.tld"
  dns:
    enabled: true
    port: 53
  http:
    port: 80
  https:
    enabled: true
    port: 443
    autocert: true
    cache_dir: "/var/lib/hookd/certs"
  api:
    auth_token: ""              # auto-generated if empty

eviction:
  interaction_ttl: "1h"
  hook_ttl: "24h"
  max_per_hook: 1000
  max_memory_mb: 1800

observability:
  metrics_enabled: true
  log_level: "info"
  log_format: "json"
```

<details>
<summary>CLI flags</summary>

| Flag | Description | Default |
|---|---|---|
| `--config` | Path to YAML config file | (none) |
| `--domain` | Override domain | (from config) |
| `--dns-port` | Override DNS port | `53` |
| `--http-port` | Override HTTP port | `80` |
| `--https-port` | Override HTTPS port | `443` |
| `--auth-token` | Override auth token | (from config) |
| `--log-level` | Log level | `info` |
| `--log-format` | Log format (`json` or `text`) | `json` |

</details>

## API

### `POST /register`

Create one or more hooks.

```bash
# Single hook
curl -X POST https://hookd.example.com/register \
  -H "X-API-Key: YOUR_TOKEN"

# Multiple hooks
curl -X POST https://hookd.example.com/register \
  -H "X-API-Key: YOUR_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"count": 5}'
```

<details>
<summary>Response example</summary>

```json
{
  "id": "abc123",
  "dns": "abc123.hookd.example.com",
  "http": "http://abc123.hookd.example.com",
  "https": "https://abc123.hookd.example.com",
  "created_at": "2025-10-01T10:30:00Z"
}
```

</details>

### `GET /poll/:id`

Retrieve and clear all interactions for a hook.

```bash
curl https://hookd.example.com/poll/abc123 \
  -H "X-API-Key: YOUR_TOKEN"
```

<details>
<summary>Response example</summary>

```json
{
  "interactions": [
    {
      "id": "int_xyz",
      "type": "dns",
      "timestamp": "2025-10-01T10:31:00Z",
      "source_ip": "1.2.3.4",
      "data": {
        "qname": "abc123.hookd.example.com",
        "qtype": "A"
      }
    },
    {
      "id": "int_abc",
      "type": "http",
      "timestamp": "2025-10-01T10:32:00Z",
      "source_ip": "5.6.7.8",
      "data": {
        "method": "POST",
        "path": "/callback",
        "headers": {},
        "body": "payload"
      }
    }
  ]
}
```

</details>

### `POST /poll`

Batch poll multiple hooks in one request.

```bash
curl -X POST https://hookd.example.com/poll \
  -H "X-API-Key: YOUR_TOKEN" \
  -H "Content-Type: application/json" \
  -d '["abc123", "def456"]'
```

### `GET /metrics`

Server statistics (no authentication required).

```bash
curl https://hookd.example.com/metrics
```

## Clients

| Language | Install | Documentation |
|----------|---------|---------------|
| Go | `go get github.com/JoshuaMart/Hookd/clients/go` | [README](./clients/go/README.md) |
| Ruby | `gem install hookd-client` | [README](./clients/ruby/README.md) |
| cURL | Built-in | See examples above |

```ruby
require 'hookd'

client = Hookd::Client.new(server: "https://hookd.example.com", token: ENV['HOOKD_TOKEN'])
hook = client.register
interactions = client.poll(hook.id)
```

```go
client := hookd.NewClient("https://hookd.example.com", "YOUR_TOKEN")
hook, _ := client.Register()
interactions, _ := client.Poll(hook.ID)
```

## Documentation

- **[Server Setup & Configuration](./server/README.md)** — deployment, systemd, architecture, troubleshooting
- **[Go Client](./clients/go/README.md)** — Go client library API reference
- **[Ruby Client](./clients/ruby/README.md)** — Ruby client library API reference

## License

[MIT](LICENSE)
