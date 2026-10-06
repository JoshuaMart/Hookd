package hookd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

func (c *Client) get(path string) (map[string]any, error) {
	return c.request(http.MethodGet, path, nil)
}

func (c *Client) delete(path string) (map[string]any, error) {
	return c.request(http.MethodDelete, path, nil)
}

func (c *Client) post(path string, body any) (map[string]any, error) {
	var bodyReader io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return nil, &Error{Message: fmt.Sprintf("failed to marshal request body: %v", err)}
		}
		bodyReader = bytes.NewReader(jsonBody)
	}

	return c.request(http.MethodPost, path, bodyReader)
}

// request applies authentication consistently to every endpoint.
func (c *Client) request(method, path string, body io.Reader) (map[string]any, error) {
	req, err := http.NewRequest(method, c.server+path, body)
	if err != nil {
		return nil, &ConnectionError{Message: fmt.Sprintf("failed to create request: %v", err)}
	}
	req.Header.Set("X-API-Key", c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.doRequest(req)
}

func (c *Client) doRequest(req *http.Request) (map[string]any, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &ConnectionError{Message: fmt.Sprintf("connection error: %v", err)}
	}
	defer func() { _ = resp.Body.Close() }()

	// Status errors take precedence and leave the response body unread.
	if err := responseStatusError(resp.StatusCode); err != nil {
		return nil, err
	}
	body, err := c.readResponseBody(resp.Body)
	if err != nil {
		return nil, err
	}
	return decodeResponse(body)
}

// responseStatusError preserves the public error types for HTTP failures.
func responseStatusError(status int) error {
	switch {
	case status == 401:
		return &AuthenticationError{Message: "authentication failed", StatusCode: 401}
	case status == 404:
		return &NotFoundError{Message: "resource not found", StatusCode: 404}
	case status >= 500:
		return &ServerError{Message: fmt.Sprintf("server error: %d", status), StatusCode: status}
	case status < 200 || status >= 300:
		return &Error{Message: fmt.Sprintf("unexpected status: %d", status), StatusCode: status}
	}

	return nil
}

// readResponseBody reads at most one byte beyond the configured response cap.
func (c *Client) readResponseBody(reader io.Reader) ([]byte, error) {
	// One byte past the ceiling is enough to detect an over-limit response.
	body := reader
	if c.maxResponseBytes > 0 {
		body = io.LimitReader(reader, c.maxResponseBytes+1)
	}

	respBody, err := io.ReadAll(body)
	if err != nil {
		return nil, &ConnectionError{Message: fmt.Sprintf("failed to read response: %v", err)}
	}

	if c.maxResponseBytes > 0 && int64(len(respBody)) > c.maxResponseBytes {
		return nil, &ResponseTooLargeError{
			Message: fmt.Sprintf("response exceeds %d bytes", c.maxResponseBytes),
			Limit:   c.maxResponseBytes,
		}
	}

	return respBody, nil
}

// decodeResponse checks the JSON envelope after transport and size validation.
func decodeResponse(respBody []byte) (map[string]any, error) {
	if len(respBody) == 0 {
		return nil, &Error{Message: "empty response body"}
	}

	var result map[string]any
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, &Error{Message: fmt.Sprintf("invalid JSON response: %v", err)}
	}

	return result, nil
}
