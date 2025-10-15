# Hookd

Hookd is a lightweight, memory-efficient interaction server designed to capture DNS and HTTP callbacks.

## Features

- 🚀 **High Performance** - Handles dozens of requests per second
- 💾 **Memory Efficient** - With configurable eviction
- 📊 **Observable** - Built-in metrics and structured logging
- 🎯 **Simple** - Single binary, YAML configuration, no dependencies

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
    auth_token: "" # If empty, a random token will be generated at startup

eviction:
  interaction_ttl: "1h"      # TTL for interactions
  hook_ttl: "24h"            # TTL for hooks
  max_per_hook: 1000         # Max interactions per hook
  max_memory_mb: 1800        # Memory limit

observability:
  metrics_enabled: true
  log_level: "info"
  log_format: "json"
```

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

Create a new hook.

**Request:**
```bash
curl -X POST https://hookd.domain.tld/register \
  -H "Authorization: Bearer YOUR_TOKEN"
```

**Response:**
```json
{
  "id": "abc123",
  "dns": "abc123.hookd.domain.tld",
  "http": "http://abc123.hookd.domain.tld",
  "https": "https://abc123.hookd.domain.tld",
  "created_at": "2025-10-01T10:30:00Z"
}
```

#### GET /poll/:id

Retrieve and delete interactions for a hook.

**Request:**
```bash
curl https://hookd.domain.tld/poll/abc123 \
  -H "Authorization: Bearer YOUR_TOKEN"
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
      "http": 24
    },
    "total": 36
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
  -H "Authorization: Bearer YOUR_TOKEN")

HOOK_ID=$(echo $RESPONSE | jq -r '.id')
HOOK_DNS=$(echo $RESPONSE | jq -r '.dns')

echo "Hook ID: $HOOK_ID"
echo "DNS: $HOOK_DNS"

# 2. Trigger DNS interaction
dig $HOOK_DNS

# 3. Trigger HTTP interaction
curl -X POST https://$HOOK_ID.hookd.domain.tld/callback \
  -d "test payload"

# 4. Poll interactions
curl -s https://hookd.domain.tld/poll/$HOOK_ID \
  -H "Authorization: Bearer YOUR_TOKEN" | jq
```

## CLI Options

```bash
hookd [options]

Options:
  --config PATH       Path to YAML configuration file
  --token TOKEN       Override authentication token
  --domain DOMAIN     Override server domain
  --dns-port PORT     Override DNS port
  --http-port PORT    Override HTTP port
  --https-port PORT   Override HTTPS port
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
- **Storage Manager**: In-memory storage with thread-safe operations
- **Eviction System**: Multi-strategy eviction (TTL, limit, memory pressure)

### Eviction Strategies

1. **Interactions TTL-based**: Automatically removes interactions older than configured TTL
2. **Hook TTL-based**: Removes hooks older than configured hook TTL (based on creation time)
3. **Per-hook limit**: Enforces max interactions per hook (FIFO)
4. **Memory pressure**: Emergency eviction when memory usage is high
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
