![Image](https://github.com/user-attachments/assets/89be446b-9b88-464a-bb4b-52a005969eea)

<p align="center">
    <a href="./LICENSE"><img src="https://img.shields.io/badge/license-MIT-_red.svg"></a>
    <a href="https://qlty.sh/gh/JoshuaMart/projects/Hookd"><img src="https://qlty.sh/badges/34ecedd0-170b-4fa5-8388-663432d25c6f/maintainability.svg" /></a>
    <a href="https://qlty.sh/gh/JoshuaMart/projects/Hookd"><img src="https://qlty.sh/badges/34ecedd0-170b-4fa5-8388-663432d25c6f/coverage.svg" /></a>
    <img src="https://img.shields.io/badge/golang-1.26-blue?logo=go">
</p>

Lightweight interaction server for capturing out-of-band DNS, HTTP and SMTP callbacks. Register ephemeral hooks, inject their endpoints into targets, and poll back to check for interactions — ideal for security testing, webhook debugging, and external service monitoring.

## Pipeline

```
1. Register — POST /register creates a hook with unique DNS + HTTP(S) endpoints
        |             (and a mail address when SMTP capture is enabled)
        |
        v
2. Inject — use the hook endpoints in your payloads, webhooks, or test targets
        |
        v
3. Capture — Hookd records every DNS query, HTTP request and mail hitting the hook
        |  ┌─────────────────────────────────────────────────────┐
        |  │  DNS server (UDP 53)  → captures qname, qtype, IP   │
        |  │  HTTP/HTTPS (80/443)  → captures method, target,    │
        |  │                          headers, body, IP          │
        |  │  SMTP (TCP 25)        → captures the raw message,   │
        |  │                          sender, subject, tag, IP   │
        |  └─────────────────────────────────────────────────────┘
        v
4. Poll — GET /poll/{id} retrieves captured interactions (and clears them,
           unless read with a cursor and acknowledged separately)
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
  public_ip: ""                 # DNS answer IP; auto-detected if empty
  dns:
    enabled: true
    port: 53
    bind_address: ""            # bind a single IP to coexist with a stub resolver
  http:
    port: 80
  https:
    enabled: true
    port: 443
    autocert: true
    cache_dir: "/var/lib/hookd/certs"
  smtp:
    enabled: false              # opt-in: binds port 25 and receives public mail
    port: 25
    max_message_bytes: 262144
  api:
    auth_token: ""              # auto-generated if empty, printed once on stderr

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
| `--public-ip` | Override the public IP returned in DNS answers | (auto-detected) |
| `--dns-port` | Override DNS port | `53` |
| `--dns-bind` | Override the DNS listener bind address | (all interfaces) |
| `--http-port` | Override HTTP port | `80` |
| `--https-port` | Override HTTPS port | `443` |
| `--smtp-port` | Override SMTP port | `25` |
| `--smtp-bind` | Override the SMTP listener bind address | (all interfaces) |
| `--auth-token` | Override auth token | (from config) |
| `--log-level` | Log level | `info` |
| `--log-format` | Log format (`json` or `text`) | `json` |

</details>

### `POST /register`

Create one or more hooks. The response carries `smtp` only when the mail
listener is enabled, so a deployment without one never advertises an address
that would black-hole.

```bash
# Single hook
curl -X POST https://hookd.example.com/register \
  -H "X-API-Key: YOUR_TOKEN"

# Multiple hooks
curl -X POST https://hookd.example.com/register \
  -H "X-API-Key: YOUR_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"count": 5}'

# Long-lived hook (persisted, survives restarts) with metadata
curl -X POST https://hookd.example.com/register \
  -H "X-API-Key: YOUR_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"ttl": "7d", "metadata": {"target": "acme", "field": "profile.bio"}}'
```

<details>
<summary>Response example</summary>

```json
{
  "id": "abc123",
  "dns": "abc123.hookd.example.com",
  "http": "http://abc123.hookd.example.com",
  "https": "https://abc123.hookd.example.com",
  "smtp": "abc123@hookd.example.com",
  "created_at": "2025-10-01T10:30:00Z",
  "expires_at": "2025-10-08T10:30:00Z",
  "metadata": {"target": "acme", "field": "profile.bio"}
}
```

</details>

### `GET /poll/:id`

Retrieve and clear all interactions for a hook. For a hook registered with
metadata, the response also echoes it. To read without clearing, see
[cursor reads](#get-pollidafterseq-and-delete-pollidthroughseq).

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
      "seq": 1,
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
      "seq": 2,
      "type": "http",
      "timestamp": "2025-10-01T10:32:00Z",
      "source_ip": "5.6.7.8",
      "data": {
        "method": "POST",
        "path": "/callback?token=s3cr3t",
        "headers": {},
        "body": "payload"
      }
    },
    {
      "id": "int_def",
      "seq": 3,
      "type": "smtp",
      "timestamp": "2025-10-01T10:33:00Z",
      "source_ip": "9.10.11.12",
      "data": {
        "helo": "mail.vendor.example",
        "mail_from": "signup@vendor.example",
        "rcpt_to": "abc123+vendor@hookd.example.com",
        "tag": "vendor",
        "subject": "Your verification code",
        "body": "From: signup@vendor.example\r\nSubject: Your verification code\r\n\r\nYour code is 123456."
      }
    }
  ]
}
```

</details>

### `GET /poll/:id?after=<seq>` and `DELETE /poll/:id?through=<seq>`

Every interaction carries a `seq` that increases per hook. A cursor read returns
the interactions past `after` **without deleting them**; acknowledge them once
your own storage has committed, so a crash in between loses nothing.

```bash
curl "https://hookd.example.com/poll/abc123?after=0" -H "X-API-Key: YOUR_TOKEN"
# {"interactions": [... "seq": 1 ..., ... "seq": 2 ...], "dropped_through": 0}

curl -X DELETE "https://hookd.example.com/poll/abc123?through=2" -H "X-API-Key: YOUR_TOKEN"
# {"acknowledged": 2}
```

Unacknowledged interactions still fall under the per-hook limit and TTL
eviction. `dropped_through` is the highest `seq` evicted before being
acknowledged: above your cursor, interactions were lost.

### `POST /poll`

Batch poll multiple hooks in one request. For the cursor equivalents, use
`POST /read` with `{"after": {"abc123": 2}}` and `POST /ack` with
`{"through": {"abc123": 5}}`.

```bash
curl -X POST https://hookd.example.com/poll \
  -H "X-API-Key: YOUR_TOKEN" \
  -H "Content-Type: application/json" \
  -d '["abc123", "def456"]'
```

Up to 1000 hook IDs per request.

### `GET /activity`

List the **long-lived** hooks that currently have pending interactions — so you
can discover which of your many long-lived hooks have fired without polling each
one. Read the details with `GET /poll/:id`. The list is derived from state:
a hook drops off once drained or acknowledged, and `last_seq` tells you whether
it has anything past your cursor.

```bash
curl https://hookd.example.com/activity \
  -H "X-API-Key: YOUR_TOKEN"
```

<details>
<summary>Response example</summary>

```json
{
  "hooks": [
    {
      "hook": {
        "id": "abc123",
        "dns": "abc123.hookd.example.com",
        "created_at": "2025-10-01T10:30:00Z",
        "expires_at": "2025-10-08T10:30:00Z",
        "metadata": {"target": "acme", "field": "profile.bio"}
      },
      "pending_count": 3,
      "last_interaction_at": "2025-10-03T14:12:00Z",
      "last_seq": 7
    }
  ]
}
```

</details>

### `GET /metrics`

Server statistics (no authentication required). Not mounted when
`observability.metrics_enabled` is `false`.

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
