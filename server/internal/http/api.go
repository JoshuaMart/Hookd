package http

import (
	"log/slog"

	"github.com/jomar/hookd/internal/config"
	"github.com/jomar/hookd/internal/eviction"
	"github.com/jomar/hookd/internal/storage"
)

// APIHandler handles API endpoints
type APIHandler struct {
	storage   storage.Manager
	evictor   *eviction.Evictor
	domain    string
	longLived config.LongLivedConfig
	// A hook carries a mail address only when a listener is running, so a
	// deployment without one never hands out a black hole.
	smtpEnabled bool
	logger      *slog.Logger
}

// APIHandlerOptions groups API configuration and its runtime dependencies.
type APIHandlerOptions struct {
	Storage     storage.Manager
	Evictor     *eviction.Evictor
	Domain      string
	LongLived   config.LongLivedConfig
	SMTPEnabled bool
	Logger      *slog.Logger
}

// NewAPIHandler creates a new API handler.
func NewAPIHandler(opts APIHandlerOptions) *APIHandler {
	return &APIHandler{
		storage:     opts.Storage,
		evictor:     opts.Evictor,
		domain:      opts.Domain,
		longLived:   opts.LongLived,
		smtpEnabled: opts.SMTPEnabled,
		logger:      opts.Logger,
	}
}
