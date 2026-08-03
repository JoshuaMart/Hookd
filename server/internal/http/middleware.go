package http

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
)

// RequireTLSMiddleware refuses authenticated API requests sent in plaintext.
// Redirecting instead would leak the token on every call — it is already on the
// wire — since clients keep their http:// base URL. enforce must only be set
// when an HTTPS listener is actually serving, or the API becomes unreachable.
func RequireTLSMiddleware(enforce bool, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !enforce || r.TLS != nil {
				next.ServeHTTP(w, r)
				return
			}

			// A request carrying a key just exposed it; one without is a bot.
			if r.Header.Get("X-API-Key") != "" {
				logger.Warn("api key received over plaintext, consider rotating it",
					"path", r.URL.Path, "client", r.RemoteAddr)
			}

			// RFC 9110: a 426 must advertise what to upgrade to.
			w.Header().Set("Upgrade", "TLS/1.3, HTTP/1.1")
			w.Header().Set("Connection", "Upgrade")
			respondJSON(w, http.StatusUpgradeRequired, map[string]string{
				"error": "API requires HTTPS",
			})
		})
	}
}

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

			// Validate token using a constant-time comparison to avoid
			// leaking the token through response timing.
			if subtle.ConstantTimeCompare([]byte(apiKey), []byte(token)) != 1 {
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
