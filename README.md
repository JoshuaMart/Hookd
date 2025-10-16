![Image](https://github.com/user-attachments/assets/89be446b-9b88-464a-bb4b-52a005969eea)

<p align="center">
    <a href="https://opensource.org/licenses/MIT"><img src="https://img.shields.io/badge/license-MIT-_red.svg"></a>
    <a href="https://qlty.sh/gh/JoshuaMart/projects/Hookd"><img src="https://qlty.sh/badges/34ecedd0-170b-4fa5-8388-663432d25c6f/maintainability.svg" /></a>
    <a href="https://qlty.sh/gh/JoshuaMart/projects/Hookd"><img src="https://qlty.sh/badges/34ecedd0-170b-4fa5-8388-663432d25c6f/coverage.svg" /></a>
</p>

A lightweight, high-performance interaction server for capturing DNS and HTTP callbacks. Perfect for security testing, debugging webhooks, and monitoring external service integrations.

## Overview

Hookd provides ephemeral endpoints that capture and store DNS queries and HTTP requests. Each registered hook gets unique DNS and HTTP(S) endpoints that can be used to detect out-of-band interactions.

### Key Features

- 🚀 **High Performance** - Handles dozens of requests per second
- 💾 **Memory Efficient** - With configurable eviction
- 📊 **Observable** - Built-in metrics and structured logging
- 🎯 **Simple** - Single binary, YAML configuration, no dependencies

## Architecture

Hookd consists of two main components:

### Server

The core interaction server written in Go that captures DNS and HTTP callbacks.

**[📖 Server Documentation](./server/README.md)**

Features:
- DNS server (port 53)
- HTTP/HTTPS server with wildcard vhost
- RESTful API for hook management
- Automatic Let's Encrypt TLS certificates
- Multi-strategy eviction system
- Real-time metrics endpoint

### Clients

Client libraries for interacting with the Hookd server:

#### Ruby Client

Ruby client for seamless integration into Ruby applications and security testing tools.

**[📖 Ruby Client Documentation](./clients/ruby/README.md)**

```ruby
require 'hookd'

client = Hookd::Client.new(
  server: "https://hookd.example.com",
  token: ENV['HOOKD_TOKEN']
)

hook = client.register
puts "DNS: #{hook.dns}"
puts "HTTP: #{hook.http}"

interactions = client.poll(hook.id)
```

## Quick Start

### 1. Deploy the Server

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
sudo cp server/config.example.yaml /etc/hookd/config.yaml

# Create certs directory (for Let's Encrypt)
sudo mkdir -p /var/lib/hookd/certs

# Run
sudo hookd --config /etc/hookd/config.yaml
```

### 2. Install a Client

**Ruby:**
```bash
gem install hookd-client
```

**cURL:**
```bash
# Register hook
curl -X POST https://hookd.example.com/register \
  -H "Authorization: Bearer YOUR_TOKEN"

# Poll interactions
curl https://hookd.example.com/poll/HOOK_ID \
  -H "Authorization: Bearer YOUR_TOKEN"
```

## API Reference

### Core Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/register` | POST | Create a new hook with DNS/HTTP endpoints |
| `/poll/:id` | GET | Retrieve and delete interactions for a hook |
| `/metrics` | GET | Get server statistics (public) |

### Response Format

**Register (single hook):**
```json
{
  "id": "abc123",
  "dns": "abc123.hookd.example.com",
  "http": "http://abc123.hookd.example.com",
  "https": "https://abc123.hookd.example.com",
  "created_at": "2025-10-01T10:30:00Z"
}
```

**Register (multiple hooks):**
```bash
curl -X POST https://hookd.example.com/register \
  -H "Authorization: Bearer YOUR_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"count": 5}'
```

```json
{
  "hooks": [
    {
      "id": "abc123",
      "dns": "abc123.hookd.example.com",
      "http": "http://abc123.hookd.example.com",
      "https": "https://abc123.hookd.example.com",
      "created_at": "2025-10-01T10:30:00Z"
    },
    {
      "id": "def456",
      "dns": "def456.hookd.example.com",
      ...
    }
  ]
}
```

**Poll:**
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
        "headers": {...},
        "body": "payload"
      }
    }
  ]
}
```

## Documentation

- **[Server Setup & Configuration](./server/README.md)** - Complete server deployment guide
- **[Ruby Client API](./clients/ruby/README.md)** - Ruby client documentation and examples

## Contributing

Contributions are welcome! Please feel free to submit issues and pull requests.

## License

MIT License - see [LICENSE](./LICENSE) for details
