// Package hookd provides a Go client for the Hookd interaction logging server.
package hookd

import (
	"net/http"
	"time"
)

const Version = "1.3.0"

// DefaultMaxResponseBytes caps the buffered response body. It is generous
// because a poll can legitimately return many interactions with full bodies.
const DefaultMaxResponseBytes int64 = 64 << 20 // 64 MiB

// Client communicates with a Hookd server.
type Client struct {
	server           string
	token            string
	httpClient       *http.Client
	maxResponseBytes int64
}

// NewClient creates a new Hookd client.
func NewClient(server, token string) *Client {
	return &Client{
		server: server,
		token:  token,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				ResponseHeaderTimeout: 10 * time.Second,
			},
		},
		maxResponseBytes: DefaultMaxResponseBytes,
	}
}

// SetMaxResponseBytes overrides the response cap. Zero or less disables it.
func (c *Client) SetMaxResponseBytes(n int64) {
	c.maxResponseBytes = n
}
