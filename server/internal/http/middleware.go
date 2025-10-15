package http

import (
	"log/slog"
	"net/http"
	"strings"
)

// AuthMiddleware creates an authentication middleware
func AuthMiddleware(token string, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Extract token from Authorization header
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				logger.Debug("missing authorization header", "path", r.URL.Path, "client", r.RemoteAddr)
				respondJSON(w, http.StatusUnauthorized, map[string]string{
					"error": "Invalid or missing authentication token",
				})
				return
			}

			// Check for Bearer token format
			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) != 2 || parts[0] != "Bearer" {
				logger.Debug("invalid authorization format", "path", r.URL.Path, "client", r.RemoteAddr)
				respondJSON(w, http.StatusUnauthorized, map[string]string{
					"error": "Invalid or missing authentication token",
				})
				return
			}

			// Validate token
			if parts[1] != token {
				logger.Debug("invalid token", "path", r.URL.Path, "client", r.RemoteAddr)
				respondJSON(w, http.StatusUnauthorized, map[string]string{
					"error": "Invalid or missing authentication token",
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
