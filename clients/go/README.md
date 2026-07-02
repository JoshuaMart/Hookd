# Hookd Go Client

Go client library for [Hookd](https://github.com/JoshuaMart/hookd/server), a DNS/HTTP interaction server for security testing and debugging.

## Installation

```bash
go get github.com/JoshuaMart/Hookd/clients/go
```

## Usage

### Basic Example

```go
package main

import (
	"fmt"
	"net/http"
	"os"

	hookd "github.com/JoshuaMart/Hookd/clients/go"
)

func main() {
	// Initialize the client
	client := hookd.NewClient("https://hookd.example.com", os.Getenv("HOOKD_TOKEN"))

	// Register a new hook
	hooks, err := client.Register(0)
	if err != nil {
		panic(err)
	}
	hook := hooks[0]
	fmt.Printf("DNS endpoint: %s\n", hook.DNS)
	fmt.Printf("HTTP endpoint: %s\n", hook.HTTP)
	fmt.Printf("HTTPS endpoint: %s\n", hook.HTTPS)

	// Make a request to the HTTP endpoint to simulate an interaction
	http.Get(hook.HTTP)

	// Poll for interactions
	interactions, err := client.Poll(hook.ID)
	if err != nil {
		panic(err)
	}
	for _, interaction := range interactions {
		if interaction.IsDNS() {
			fmt.Printf("DNS query: %v\n", interaction.Data)
		} else if interaction.IsHTTP() {
			fmt.Printf("HTTP request: %v\n", interaction.Data)
		}
	}
}
```

### Batch Polling Example

When working with multiple hooks, use `PollBatch` for better performance:

```go
// Register multiple hooks
hooks, err := client.Register(5)
if err != nil {
    panic(err)
}

// Collect hook IDs
hookIDs := make([]string, len(hooks))
for i, h := range hooks {
    hookIDs[i] = h.ID
}

// Batch poll all hooks at once (1 HTTP request instead of 5)
results, err := client.PollBatch(hookIDs)
if err != nil {
    panic(err)
}

// Display results
for hookID, result := range results {
    if result.Error != "" {
        fmt.Printf("Hook %s: %s\n", hookID, result.Error)
    } else {
        fmt.Printf("Hook %s: %d interaction(s)\n", hookID, len(result.Interactions))
        for _, interaction := range result.Interactions {
            if interaction.IsDNS() {
                fmt.Printf("  - DNS: %v\n", interaction.Data["qname"])
            } else if interaction.IsHTTP() {
                fmt.Printf("  - HTTP: %s %s\n", interaction.Data["method"], interaction.Data["path"])
            }
        }
    }
}
```

### Long-lived Hooks Example

For asynchronous detection (e.g. stored XSS), register a **long-lived** hook with
a `TTL` above the server's ephemeral `hook_ttl`. It is persisted and survives
restarts. Attach `Metadata` to correlate a fired hook back to its injection
point, then use `Activity` to discover which hooks fired without polling each.

```go
// Register a durable hook that lives for 7 days, tagged with where it was injected
hooks, err := client.RegisterHooks(hookd.RegisterOptions{
    TTL:      "7d", // or a Go duration like "168h"
    Metadata: map[string]any{"target": "acme", "field": "profile.bio"},
})
if err != nil {
    panic(err)
}
fmt.Printf("Hook %s expires at %s\n", hooks[0].ID, hooks[0].ExpiresAt)

// ... later, discover which long-lived hooks have fired ...
activity, err := client.Activity()
if err != nil {
    panic(err)
}
for _, a := range activity {
    fmt.Printf("Hook %s fired %d time(s), meta=%v\n", a.Hook.ID, a.PendingCount, a.Hook.Metadata)
    interactions, _ := client.Poll(a.Hook.ID) // drain the details
    _ = interactions
}
```

### Configuration

The client requires two parameters:

- `server`: The Hookd server URL (e.g., `https://hookd.example.com`)
- `token`: Authentication token for API access

```go
client := hookd.NewClient(server, token)
```

### API Reference

#### `NewClient(server, token string) *Client`

Creates a new Hookd client with default timeouts (10s connect, 30s total).

#### `(*Client) Register(count int) ([]Hook, error)`

Register one or more hooks and get DNS/HTTP endpoints.

```go
// Single hook
hooks, _ := client.Register(0)
hook := hooks[0]

// Multiple hooks
hooks, _ := client.Register(5)
```

Parameters:
- `count` - Number of hooks to create (0 or 1 for single, >1 for multiple)

Returns: Slice of `Hook` objects

#### `(*Client) RegisterHooks(opts RegisterOptions) ([]Hook, error)`

Register hooks with options: `Count`, `TTL` (a Go duration like `"168h"` or a day
count like `"7d"` — a value above the server's `hook_ttl` creates a durable
long-lived hook), and `Metadata` (stored with the hook, echoed on poll).

```go
hooks, _ := client.RegisterHooks(hookd.RegisterOptions{TTL: "7d", Metadata: map[string]any{"k": "v"}})
```

#### `(*Client) Activity() ([]HookActivity, error)`

List the long-lived hooks that currently have pending interactions. Returns an
empty slice when none have fired (or long-lived hooks are disabled server-side).

```go
activity, _ := client.Activity()
```

#### `(*Client) Poll(hookID string) ([]Interaction, error)`

Poll for interactions captured by a single hook.

```go
interactions, err := client.Poll("abc123")
```

Returns: Slice of `Interaction` objects

#### `(*Client) PollBatch(hookIDs []string) (map[string]BatchResult, error)`

Batch poll for interactions from multiple hooks in a single request.

```go
results, err := client.PollBatch([]string{"abc123", "def456"})
for hookID, result := range results {
    fmt.Printf("%s: %d interactions, error: %s\n", hookID, len(result.Interactions), result.Error)
}
```

Returns: Map of hook ID to `BatchResult` (containing `Interactions` and `Error`)

#### `(*Client) Metrics() (Metrics, error)`

Get server metrics.

```go
metrics, err := client.Metrics()
fmt.Printf("Total hooks: %v\n", metrics["total_hooks"])
```

### Types

#### `Hook`

| Field       | Type   | Description          |
|-------------|--------|----------------------|
| `ID`        | string | Unique hook identifier |
| `DNS`       | string | DNS endpoint         |
| `HTTP`      | string | HTTP endpoint        |
| `HTTPS`     | string | HTTPS endpoint       |
| `CreatedAt` | string | Creation timestamp   |
| `ExpiresAt` | string | Expiry timestamp (long-lived hooks) |
| `Metadata`  | map[string]any | Metadata attached at registration |

#### `HookActivity`

| Field               | Type   | Description                          |
|---------------------|--------|--------------------------------------|
| `Hook`              | Hook   | The long-lived hook that fired       |
| `PendingCount`      | int    | Number of interactions awaiting poll |
| `LastInteractionAt` | string | Timestamp of the most recent one     |

#### `Interaction`

| Field      | Type            | Description                    |
|------------|-----------------|--------------------------------|
| `Type`     | string          | "dns" or "http"                |
| `Timestamp`| string          | When the interaction occurred  |
| `SourceIP` | string          | Source IP address               |
| `Data`     | map[string]any  | Interaction details            |

Methods:
- `IsDNS() bool` - Returns true if this is a DNS interaction
- `IsHTTP() bool` - Returns true if this is an HTTP interaction

#### `BatchResult`

| Field          | Type           | Description                     |
|----------------|----------------|---------------------------------|
| `Interactions` | []Interaction  | Captured interactions           |
| `Error`        | string         | Error message (empty if none)   |

### Error Handling

The client returns typed errors for different failure conditions:

```go
_, err := client.Register(0)
if err != nil {
    switch err.(type) {
    case *hookd.AuthenticationError:
        fmt.Println("Invalid token")
    case *hookd.NotFoundError:
        fmt.Println("Resource not found")
    case *hookd.ServerError:
        fmt.Println("Server error")
    case *hookd.ConnectionError:
        fmt.Println("Connection failed")
    default:
        fmt.Printf("Error: %v\n", err)
    }
}
```

Error types:
- `*Error` - Base error type
- `*AuthenticationError` - 401 Unauthorized
- `*NotFoundError` - 404 Not Found
- `*ServerError` - 5xx Server Error
- `*ConnectionError` - Network/timeout errors
