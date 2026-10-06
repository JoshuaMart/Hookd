package http

import (
	"net/http"
)

// routeByHost sends hook subdomains to the capture handler, everything else to
// the API mux. On paths alone a callback to /register would get a 401 from the
// API and never be recorded.
func routeByHost(apiMux http.Handler, capture *CaptureHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture.extractHookID(r.Host) != "" {
			capture.ServeHTTP(w, r)
			return
		}
		apiMux.ServeHTTP(w, r)
	})
}

// newHandler builds the shared HTTP/HTTPS routing and middleware.
func (s *Server) newHandler() http.Handler {
	// Create handlers
	apiHandler := NewAPIHandler(APIHandlerOptions{
		Storage:     s.storage,
		Evictor:     s.evictor,
		Domain:      s.config.Domain,
		LongLived:   s.longLived,
		SMTPEnabled: s.config.SMTP.Enabled,
		Logger:      s.logger,
	})
	captureHandler := NewCaptureHandler(s.storage, s.config.Domain, s.logger, s.idGenerator, s.evictor.MaxInteractionBodyBytes())

	// Create main mux
	mux := http.NewServeMux()

	// API endpoints. TLS is enforced ahead of auth, on the same condition as the
	// HTTPS listener below — enforcing it without one would strand the API.
	authMW := AuthMiddleware(s.config.API.AuthToken, s.logger)
	tlsMW := RequireTLSMiddleware(s.config.HTTPS.Enabled && s.config.HTTPS.AutoCert, s.logger)
	apiMW := func(h http.Handler) http.Handler { return tlsMW(authMW(h)) }

	mux.Handle("/register", apiMW(http.HandlerFunc(apiHandler.HandleRegister)))
	mux.Handle("/poll", apiMW(http.HandlerFunc(apiHandler.HandlePollBatch)))
	mux.Handle("/poll/", apiMW(http.HandlerFunc(apiHandler.HandlePoll)))
	mux.Handle("/read", apiMW(http.HandlerFunc(apiHandler.HandleRead)))
	mux.Handle("/ack", apiMW(http.HandlerFunc(apiHandler.HandleAck)))
	mux.Handle("/activity", apiMW(http.HandlerFunc(apiHandler.HandleActivity)))
	mux.Handle("/hooks", apiMW(http.HandlerFunc(apiHandler.HandleHooks)))

	// Liveness endpoints stay public, including on HTTP when API TLS is enforced.
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/healthz", handleHealth)

	// Metrics endpoint, left unmounted when disabled.
	if s.observability.MetricsEnabled {
		var metricsHandler http.Handler = http.HandlerFunc(apiHandler.HandleMetrics)
		if s.observability.MetricsRequireAuth {
			metricsHandler = apiMW(metricsHandler)
		}
		mux.Handle("/metrics", metricsHandler)
	}

	// Wildcard capture (everything else)
	mux.Handle("/", captureHandler)

	// Apply global middleware
	return RecoveryMiddleware(s.logger)(LoggingMiddleware(s.logger)(routeByHost(mux, captureHandler)))
}

// handleHealth reports HTTP liveness; it does not probe DNS, SMTP, or storage.
func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		respondJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
		return
	}
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
