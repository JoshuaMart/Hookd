package http

import (
	"net/http"

	"github.com/jomar/hookd/internal/config"
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

// newHandlers builds the public routing and, when the dedicated API listener
// is enabled, the private one; private is nil otherwise.
func (s *Server) newHandlers() (public, private http.Handler) {
	api := NewAPIHandler(APIHandlerOptions{
		Storage:     s.storage,
		Evictor:     s.evictor,
		Domain:      s.config.Domain,
		LongLived:   s.longLived,
		SMTPEnabled: s.config.SMTP.Enabled,
		OwnerSecret: s.config.API.OwnerSecret.Enabled,
		Logger:      s.logger,
	})
	capture := NewCaptureHandler(s.storage, s.config.Domain, s.logger, s.idGenerator, s.evictor.MaxInteractionBodyBytes())

	if !s.config.API.Listener.Enabled {
		return s.publicHandler(api, capture), nil
	}
	return s.publicHandler(nil, capture), s.privateHandler(api)
}

// publicHandler serves capture and liveness, plus the API when api is set.
// Without it, API paths fall through to capture and disclose nothing.
func (s *Server) publicHandler(api *APIHandler, capture *CaptureHandler) http.Handler {
	mux := http.NewServeMux()
	if api != nil {
		// TLS is enforced on the same condition as the HTTPS listener —
		// enforcing it without one would strand the API.
		s.registerAPIRoutes(mux, api, s.config.HTTPS.Enabled && s.config.HTTPS.AutoCert)
	}
	registerHealthRoutes(mux)
	mux.Handle("/", capture)
	return s.withGlobalMiddleware(routeByHost(mux, capture))
}

// privateHandler serves the API, metrics and liveness of the dedicated
// listener. It never captures, whatever the Host.
func (s *Server) privateHandler(api *APIHandler) http.Handler {
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux, api, s.config.API.Listener.TLS != config.APIListenerTLSNone)
	registerHealthRoutes(mux)
	mux.HandleFunc("/", handleNotFound)
	return s.withGlobalMiddleware(mux)
}

// registerAPIRoutes mounts the authenticated endpoints and /metrics. TLS is
// enforced ahead of auth when requireTLS is set.
func (s *Server) registerAPIRoutes(mux *http.ServeMux, api *APIHandler, requireTLS bool) {
	authMW := AuthMiddleware(s.config.API.AuthToken, s.logger)
	tlsMW := RequireTLSMiddleware(requireTLS, s.logger)
	apiMW := func(h http.Handler) http.Handler { return tlsMW(authMW(h)) }

	mux.Handle("/register", apiMW(http.HandlerFunc(api.HandleRegister)))
	mux.Handle("/poll", apiMW(http.HandlerFunc(api.HandlePollBatch)))
	mux.Handle("/poll/", apiMW(http.HandlerFunc(api.HandlePoll)))
	mux.Handle("/read", apiMW(http.HandlerFunc(api.HandleRead)))
	mux.Handle("/ack", apiMW(http.HandlerFunc(api.HandleAck)))
	mux.Handle("/activity", apiMW(http.HandlerFunc(api.HandleActivity)))
	mux.Handle("/hooks", apiMW(http.HandlerFunc(api.HandleHooks)))

	// Metrics endpoint, left unmounted when disabled.
	if s.observability.MetricsEnabled {
		var metricsHandler http.Handler = http.HandlerFunc(api.HandleMetrics)
		if s.observability.MetricsRequireAuth {
			metricsHandler = apiMW(metricsHandler)
		}
		mux.Handle("/metrics", metricsHandler)
	}
}

// registerHealthRoutes mounts liveness, which stays unauthenticated and is
// served over HTTP even when API TLS is enforced.
func registerHealthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/healthz", handleHealth)
}

// withGlobalMiddleware applies panic recovery and request logging.
func (s *Server) withGlobalMiddleware(h http.Handler) http.Handler {
	return RecoveryMiddleware(s.logger)(LoggingMiddleware(s.logger)(h))
}

// handleNotFound answers unknown paths on the private listener.
func handleNotFound(w http.ResponseWriter, _ *http.Request) {
	respondJSON(w, http.StatusNotFound, map[string]string{"error": "Not found"})
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
