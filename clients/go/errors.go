package hookd

// Error is the base error type for Hookd client errors.
type Error struct {
	Message    string
	StatusCode int
}

func (e *Error) Error() string { return e.Message }

// AuthenticationError indicates a 401 response.
type AuthenticationError struct {
	Message    string
	StatusCode int
}

func (e *AuthenticationError) Error() string { return e.Message }

// NotFoundError indicates a 404 response.
type NotFoundError struct {
	Message    string
	StatusCode int
}

func (e *NotFoundError) Error() string { return e.Message }

// ServerError indicates a 5xx response.
type ServerError struct {
	Message    string
	StatusCode int
}

func (e *ServerError) Error() string { return e.Message }

// ConnectionError indicates a network or timeout failure.
type ConnectionError struct {
	Message    string
	StatusCode int
}

func (e *ConnectionError) Error() string { return e.Message }

// ResponseTooLargeError indicates a response above the client's size limit.
type ResponseTooLargeError struct {
	Message string
	Limit   int64
}

func (e *ResponseTooLargeError) Error() string { return e.Message }
