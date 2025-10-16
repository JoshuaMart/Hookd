package http

import (
	"log/slog"
	"net/http"
)

// AuthMiddleware creates an authentication middleware
func AuthMiddleware(token string, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Extract token from X-API-Key header
			apiKey := r.Header.Get("X-API-Key")
			if apiKey == "" {
				logger.Debug("missing api key", "path", r.URL.Path, "client", r.RemoteAddr)
				respondJSON(w, http.StatusUnauthorized, map[string]string{
					"error": "Invalid or missing API key",
				})
				return
			}

			// Validate token
			if apiKey != token {
				logger.Debug("invalid api key", "path", r.URL.Path, "client", r.RemoteAddr)
				respondJSON(w, http.StatusUnauthorized, map[string]string{
					"error": "Invalid or missing API key",
				})
				return
			}

			// Token is valid, proceed
			next.ServeHTTP(w, r)
		})
	}
}

// LoggingMiddleware logs HTTP requests
func LoggingMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			logger.Debug("http request",
				"method", r.Method,
				"path", r.URL.Path,
				"client", r.RemoteAddr,
				"user_agent", r.UserAgent())

			next.ServeHTTP(w, r)
		})
	}
}

// RecoveryMiddleware recovers from panics
func RecoveryMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if err := recover(); err != nil {
					logger.Error("panic recovered",
						"error", err,
						"path", r.URL.Path,
						"method", r.Method)

					respondJSON(w, http.StatusInternalServerError, map[string]string{
						"error": "Internal server error",
					})
				}
			}()

			next.ServeHTTP(w, r)
		})
	}
}
