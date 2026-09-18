# Hookd

Hookd is a lightweight, memory-efficient interaction server designed to capture DNS, HTTP and SMTP callbacks.

## Features

- 🚀 **High Performance** - Handles dozens of requests per second
- 💾 **Memory Efficient** - With configurable eviction
- 📊 **Observable** - Built-in metrics and structured logging
- 🎯 **Simple** - Single binary, YAML configuration, no dependencies
- 📬 **Disposable mailboxes** - Optional inbound SMTP capture, reception only

## Quick Start

### Installation

**Available binaries:**
- `hookd-linux-amd64` - Linux x86_64
- `hookd-linux-arm64` - Linux ARM64
- `hookd-darwin-amd64` - macOS Intel
- `hookd-darwin-arm64` - macOS Apple Silicon

Download from [latest build artifacts](https://github.com/JoshuaMart/Hookd/actions/workflows/build.yml).

```bash
chmod +x hookd-linux-amd64
sudo mv hookd-linux-amd64 /usr/local/bin/hookd

# Create config directory
sudo mkdir -p /etc/hookd
sudo cp config.example.yaml /etc/hookd/config.yaml

# Create certs directory (for Let's Encrypt)
sudo mkdir -p /var/lib/hookd/certs
```

### Configuration

Edit `/etc/hookd/config.yaml`:

```yaml
server:
  domain: "hookd.domain.tld"  # Your domain
  public_ip: ""               # DNS answer IP; auto-detected if empty (set behind NAT)
  dns:
    enabled: true
    port: 53
    bind_address: ""          # bind a single IP to coexist with a stub resolver
  http:
    port: 80
  https:
    enabled: true
    port: 443
    autocert: true
    cache_dir: "/var/lib/hookd/certs"
  smtp:
    enabled: false              # Opt-in: binds port 25 and receives public mail
    port: 25
    bind_address: ""            # Empty binds all interfaces
    max_message_bytes: 262144   # Advertised via SIZE; above it, 552
    max_recipients: 10          # Past it, 452
    max_concurrent: 50          # Past it, 421
    read_timeout: "60s"         # One command or data line
    session_timeout: "5m"       # A whole session
  api:
    auth_token: "" # If empty, a random token will be generated at startup

eviction:
  interaction_ttl: "1h"      # TTL for interactions
  hook_ttl: "24h"            # TTL for hooks (ephemeral threshold)
  max_per_hook: 1000         # Max interactions per hook
  max_memory_mb: 1800        # Memory limit

long_lived:
  enabled: true                       # Persist long-lived hooks to SQLite
  max_ttl: "720h"                     # Absolute cap on a long-lived ttl
  max_hooks: 500                      # Max long-lived hooks (429 past this)
  max_interaction_body_bytes: 65536   # Persisted body truncation
  max_metadata_bytes: 8192            # Max per-hook metadata size
  db_path: "/var/lib/hookd/longlived.db"

observability:
  metrics_enabled: true
  log_level: "info"
  log_format: "json"
```

### Ephemeral vs long-lived hooks

By default hooks are **ephemeral**: kept in memory and evicted after
`eviction.hook_ttl`. A registration whose `ttl` exceeds `hook_ttl` is a
**long-lived** hook, persisted to the SQLite database at `long_lived.db_path` so
it survives restarts — the right model for stored-XSS detection, where a payload
may fire days after injection and an in-memory-only server would drop the
interaction silently after a restart. Long-lived hooks are bounded by
`long_lived.max_ttl` and `long_lived.max_hooks`; discover which ones have fired
with `GET /activity`, then drain them with `GET /poll/:id`.

### Mail capture (optional)

With `server.smtp.enabled`, a registered hook also answers at
`<hookid>@hookd.domain.tld`, and at `<hookid>+anylabel@hookd.domain.tld` for
free — sub-addressing needs no extra API call, and the label is stored on the
interaction so you can see which site leaked the address. Mail arrives as an
ordinary interaction of type `smtp`, read back with `GET /poll/:id`.

Reception only: Hookd never sends, relays or bounces. A recipient outside the
domain gets `550 5.7.1`; every address under it gets a uniform `250` whether or
not a hook exists, so the server is neither an open relay nor an enumeration
oracle for live hook IDs. There is no SPF/DKIM/DMARC verification — the raw
message is the signal.

Deploying it needs nothing in DNS (the server already answers MX for every name
under the domain, pointing at itself), but port 25 is privileged — grant
`CAP_NET_BIND_SERVICE` as for port 53 — and some hosting providers filter
inbound 25.

### DNS Setup

Configure your DNS records:

```
hookd.domain.tld.        A       YOUR_SERVER_IP
hookd.domain.tld.        NS      hookd.domain.tld.
```

### Running

```bash
# Start the server
sudo hookd --config /etc/hookd/config.yaml

# Output:
# {"level":"info","time":"2025-10-01T10:00:00Z","msg":"auth token generated","token":"a1b2c3d4e5f6g7h8"}
# {"level":"info","time":"2025-10-01T10:00:00Z","msg":"hookd starting","version":"0.1.0","domain":"hookd.domain.tld"}
# {"level":"info","time":"2025-10-01T10:00:00Z","msg":"dns server starting","port":53}
# {"level":"info","time":"2025-10-01T10:00:00Z","msg":"http server starting","port":80}
# {"level":"info","time":"2025-10-01T10:00:00Z","msg":"https server starting (autocert)","port":443}
```

### Systemd Service

Create `/etc/systemd/system/hookd.service`:

```ini
[Unit]
Description=Hookd Interaction Server
After=network.target

[Service]
Type=simple
User=root
ExecStart=/usr/local/bin/hookd --config /etc/hookd/config.yaml
Restart=on-failure
RestartSec=5s
LimitNOFILE=65536
MemoryMax=2G

[Install]
WantedBy=multi-user.target
```

Enable and start:

```bash
sudo systemctl daemon-reload
sudo systemctl enable hookd
sudo systemctl start hookd
sudo systemctl status hookd
```

## Usage

### API Endpoints

#### POST /register

Create one or more hooks.

**Request (single hook):**
```bash
curl -X POST https://hookd.domain.tld/register \
  -H "X-API-Key: YOUR_TOKEN"
```

**Response (single hook):**
```json
{
  "id": "abc123",
  "dns": "abc123.hookd.domain.tld",
  "http": "http://abc123.hookd.domain.tld",
  "https": "https://abc123.hookd.domain.tld",
  "smtp": "abc123@hookd.domain.tld",
  "created_at": "2025-10-01T10:30:00Z"
}
```

`smtp` is present only when `server.smtp.enabled` is set: a deployment with no
mail listener does not advertise an address that would black-hole.

**Request (multiple hooks):**
```bash
curl -X POST https://hookd.domain.tld/register \
  -H "X-API-Key: YOUR_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"count": 2}'
```

**Response (multiple hooks):**
```json
{
  "hooks": [
    {
      "id": "abc123",
      "dns": "abc123.hookd.domain.tld",
      "http": "http://abc123.hookd.domain.tld",
      "https": "https://abc123.hookd.domain.tld",
      "created_at": "2025-10-01T10:30:00Z"
    },
    {
      "id": "def456",
      "dns": "def456.hookd.domain.tld",
      "http": "http://def456.hookd.domain.tld",
      "https": "https://def456.hookd.domain.tld",
      "created_at": "2025-10-01T10:30:01Z"
    }
  ]
}
```

**Parameters:**
- `count` (optional): Number of hooks to create (default: 1)
- `ttl` (optional): Lifetime as a Go duration (`168h`) or day count (`7d`). A
  value above `eviction.hook_ttl` registers a durable **long-lived** hook
  (persisted, survives restarts), capped at `long_lived.max_ttl`. Omit for an
  ephemeral hook.
- `metadata` (optional): Arbitrary JSON object stored with the hook and echoed
  back on poll. Capped at `long_lived.max_metadata_bytes`.

Long-lived responses include a non-zero `expires_at` and echo `metadata`.
Registering past `long_lived.max_hooks` returns HTTP 429.

#### GET /activity

List the long-lived hooks that currently have pending interactions, so you can
discover which fired without polling each one. Authenticated; mutates nothing.

**Request:**
```bash
curl https://hookd.domain.tld/activity \
  -H "X-API-Key: YOUR_TOKEN"
```

**Response:**
```json
{
  "hooks": [
    {
      "hook": { "id": "abc123", "expires_at": "2025-10-08T10:30:00Z", "metadata": {"field": "profile.bio"} },
      "pending_count": 3,
      "last_interaction_at": "2025-10-03T14:12:00Z"
    }
  ]
}
```

#### GET /poll/:id

Retrieve and delete interactions for a single hook. For a hook registered with
metadata, the response also echoes it under `metadata`.

**Request:**
```bash
curl https://hookd.domain.tld/poll/abc123 \
  -H "X-API-Key: YOUR_TOKEN"
```

**Response:**
```json
{
  "interactions": [
    {
      "id": "int_xyz789",
      "type": "dns",
      "timestamp": "2025-10-01T10:31:15Z",
      "source_ip": "1.2.3.4",
      "data": {
        "qname": "abc123.hookd.domain.tld",
        "qtype": "A"
      }
    },
    {
      "id": "int_abc456",
      "type": "http",
      "timestamp": "2025-10-01T10:32:00Z",
      "source_ip": "5.6.7.8",
      "data": {
        "method": "POST",
        "path": "/callback",
        "headers": {
          "User-Agent": "curl/7.68.0"
        },
        "body": "payload data"
      }
    }
  ]
}
```

#### POST /poll

**Batch poll** - Retrieve and delete interactions for multiple hooks in a single request.

**Request:**
```bash
curl -X POST https://hookd.domain.tld/poll \
  -H "X-API-Key: YOUR_TOKEN" \
  -H "Content-Type: application/json" \
  -d '["abc123", "def456", "ghi789"]'
```

**Response:**
```json
{
  "results": {
    "abc123": {
      "interactions": [
        {
          "id": "int_xyz789",
          "type": "dns",
          "timestamp": "2025-10-01T10:31:15Z",
          "source_ip": "1.2.3.4",
          "data": {
            "qname": "abc123.hookd.domain.tld",
            "qtype": "A"
          }
        }
      ]
    },
    "def456": {
      "interactions": []
    },
    "ghi789": {
      "error": "Hook not found"
    }
  }
}
```

#### GET /metrics

Get server metrics (no authentication required).

**Request:**
```bash
curl https://hookd.domain.tld/metrics
```

**Response:**
```json
{
  "evictions": {
    "by_strategy": {
      "expired": 2,
      "hook_expired": 1,
      "memory_pressure": 0,
      "overflow": 0
    },
    "total": 3
  },
  "hooks": {
    "active": 42
  },
  "interactions": {
    "by_type": {
      "dns": 12,
      "http": 24,
      "smtp": 3
    },
    "total": 39
  },
  "memory": {
    "alloc_mb": 2,
    "heap_inuse_mb": 3,
    "sys_mb": 8,
    "gc_runs": 15
  }
}
```

### Example Usage

```bash
# 1. Register a hook
RESPONSE=$(curl -s -X POST https://hookd.domain.tld/register \
  -H "X-API-Key: YOUR_TOKEN")

HOOK_ID=$(echo $RESPONSE | jq -r '.id')
HOOK_DNS=$(echo $RESPONSE | jq -r '.dns')

echo "Hook ID: $HOOK_ID"
echo "DNS: $HOOK_DNS"

# 2. Trigger DNS interaction
dig $HOOK_DNS

# 3. Trigger HTTP interaction
curl -X POST https://$HOOK_ID.hookd.domain.tld/callback \
  -d "test payload"

# 3b. Trigger SMTP interaction (when server.smtp.enabled)
#     Use the address as-is, or with any +label to tag the source.
swaks --to "$HOOK_ID+vendor@hookd.domain.tld" --server hookd.domain.tld

# 4. Poll interactions
curl -s https://hookd.domain.tld/poll/$HOOK_ID \
  -H "X-API-Key: YOUR_TOKEN" | jq
```

## CLI Options

```bash
hookd [options]

Options:
  --config PATH       Path to YAML configuration file
  --token TOKEN       Override authentication token
  --domain DOMAIN     Override server domain
  --public-ip IP      Override the public IP returned in DNS answers
  --dns-port PORT     Override DNS port
  --dns-bind IP       Override the DNS listener bind address
  --http-port PORT    Override HTTP port
  --https-port PORT   Override HTTPS port
  --smtp-port PORT    Override SMTP port
  --smtp-bind IP      Override the SMTP listener bind address
  --version           Show version information
  --help, -h          Show help message

Examples:
  # Start with config file
  hookd --config /etc/hookd/config.yaml

  # Override token
  hookd --config config.yaml --token my-secret-token

  # Override ports
  hookd --config config.yaml --dns-port 53 --http-port 80
```

## Building from Source

### Prerequisites

- Go 1.24.7 or later

### Build

```bash
# Clone the repository
git clone https://github.com/JoshuaMart/hookd
cd hookd

# Install dependencies
go mod download

# Build
go build -o hookd cmd/hookd/main.go

# Run tests
go test ./...

# Run integration tests (requires root for DNS port 53)
sudo go test -tags=integration ./test/...
```

### Cross-compilation

```bash
# Linux AMD64
GOOS=linux GOARCH=amd64 go build -o hookd-linux-amd64 cmd/hookd/main.go

# Linux ARM64
GOOS=linux GOARCH=arm64 go build -o hookd-linux-arm64 cmd/hookd/main.go

# macOS
GOOS=darwin GOARCH=amd64 go build -o hookd-darwin-amd64 cmd/hookd/main.go
```

## Architecture

### Components

- **DNS Server**: Captures DNS queries on port 53 (UDP/TCP)
- **HTTP/HTTPS Server**: Captures HTTP requests with wildcard vhost
- **API Server**: REST API for hook management
- **Storage Manager**: Composite storage — in-memory for ephemeral hooks, SQLite
  for durable long-lived hooks — routed transparently by TTL and hook ID
- **Eviction System**: Multi-strategy eviction (TTL, limit, memory pressure)

### Eviction Strategies

Each strategy is pushed down into the store that owns the data, so the SQLite
store evicts with indexed SQL rather than loading everything into memory.

1. **Interactions TTL-based**: Removes ephemeral interactions older than the
   configured TTL. Long-lived interactions are retained until polled or their
   hook expires.
2. **Hook TTL-based**: Removes hooks whose `expires_at` has passed (both stores).
3. **Per-hook limit**: Enforces max interactions per hook (oldest dropped first).
4. **Memory pressure**: Emergency eviction of the oldest in-memory hooks when
   heap usage is high (long-lived data lives on disk and is unaffected).
   - Triggers at 90% of max_memory_mb (based on heap memory in use)
   - Deletes oldest hooks (by creation time) until memory drops to 80%
   - Forces garbage collection for accurate measurements

## Security

### Authentication

- Bearer token authentication for API endpoints
- Token can be configured or auto-generated

### TLS/HTTPS

- Automatic Let's Encrypt certificate management
- Certificate caching and auto-renewal

## Monitoring

### Metrics

The `/metrics` endpoint provides:

- Active hooks count
- Total interactions (DNS + HTTP)
- Detailed memory statistics:
  - `alloc_mb`: Allocated memory still in use
  - `heap_inuse_mb`: Heap memory in use (used for eviction decisions)
  - `sys_mb`: Total memory obtained from OS
  - `gc_runs`: Number of garbage collection cycles (indicates memory activity)
- Eviction statistics by strategy

### Logs

Structured JSON logs (or text format):

```json
{
  "level": "info",
  "time": "2025-10-01T10:30:00Z",
  "msg": "hook created",
  "id": "abc123",
  "client": "1.2.3.4:12345"
}
```

## Troubleshooting

### DNS not working

- Ensure port 53 is accessible (firewall rules)
- Check DNS records are configured correctly
- Verify you have root privileges or `CAP_NET_BIND_SERVICE`

```bash
# Grant capability (alternative to root)
sudo setcap 'cap_net_bind_service=+ep' /usr/local/bin/hookd
```

### Port 53 already in use (systemd-resolved)

On hosts running `systemd-resolved`, a stub resolver listens on `127.0.0.53:53`.
Binding Hookd's DNS server to `0.0.0.0:53` (the default) collides with it, so it
is tempting to stop `systemd-resolved` — but that also removes the host's own DNS
resolution (`/etc/resolv.conf` points at the stub), which then breaks outbound
lookups such as reaching Let's Encrypt.

Instead, bind Hookd to the server's public IP so the two coexist on the same
port. `systemd-resolved` keeps `127.0.0.53:53`; Hookd owns `<public-ip>:53`:

```yaml
server:
  public_ip: "203.0.113.7"      # returned in DNS answers
  dns:
    bind_address: "203.0.113.7" # listen only on the public interface
```

With this, the host's resolver stays intact and Hookd needs no override of the
process-wide DNS resolver.

### Wrong IP in DNS answers (behind NAT)

If auto-detection returns a private address (`10.x`, `172.16–31.x`, `192.168.x`)
because the host is behind NAT or multi-homed, hooks will advertise an
unreachable IP. Set `server.public_ip` (or `--public-ip`) to the real public
address.

### HTTPS certificate issues

- Ensure port 80 is accessible (needed for Let's Encrypt challenge)
- Check domain DNS points to the server
- Verify cache directory has write permissions

```bash
sudo mkdir -p /var/lib/hookd/certs
sudo chown hookd:hookd /var/lib/hookd/certs
```

### Memory issues

- Reduce `max_memory_mb` in config
- Lower `interaction_ttl` and `hook_ttl` to evict faster
- Decrease `max_per_hook` limit

### Check logs

```bash
sudo journalctl -u hookd -f
```
