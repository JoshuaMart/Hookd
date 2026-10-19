package http

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// certCheckInterval bounds how often a handshake stats the key pair files.
const certCheckInterval = time.Minute

// certReloader serves a key pair from disk and picks up a rotated one, e.g.
// from cert-manager or an internal CA, without a restart.
type certReloader struct {
	certFile, keyFile string
	interval          time.Duration
	logger            *slog.Logger

	mu      sync.Mutex
	cert    *tls.Certificate
	modTime time.Time
	checked time.Time
}

// newCertReloader loads the key pair once, failing if it is unusable.
func newCertReloader(certFile, keyFile string, logger *slog.Logger) (*certReloader, error) {
	r := &certReloader{certFile: certFile, keyFile: keyFile, interval: certCheckInterval, logger: logger}
	modTime, err := r.latestModTime()
	if err != nil {
		return nil, fmt.Errorf("failed to load api listener certificate: %w", err)
	}
	if err := r.load(modTime); err != nil {
		return nil, err
	}
	return r, nil
}

// GetCertificate returns the current pair, reloaded when the files changed.
func (r *certReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now := time.Now(); now.Sub(r.checked) >= r.interval {
		r.checked = now
		r.reloadIfChanged()
	}
	return r.cert, nil
}

// reloadIfChanged keeps the current pair when the new one is unreadable,
// e.g. half written: the next check retries.
func (r *certReloader) reloadIfChanged() {
	modTime, err := r.latestModTime()
	if err != nil || !modTime.After(r.modTime) {
		return
	}
	if err := r.load(modTime); err != nil {
		r.logger.Warn("api listener certificate reload failed, keeping the current one", "error", err)
		return
	}
	r.logger.Info("api listener certificate reloaded", "cert_file", r.certFile)
}

// load replaces the pair and records the files' modification time.
func (r *certReloader) load(modTime time.Time) error {
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("failed to load api listener certificate: %w", err)
	}
	r.cert, r.modTime = &cert, modTime
	return nil
}

// latestModTime follows symlinks, so a Kubernetes secret update counts.
func (r *certReloader) latestModTime() (time.Time, error) {
	var latest time.Time
	for _, path := range []string{r.certFile, r.keyFile} {
		info, err := os.Stat(path)
		if err != nil {
			return time.Time{}, err
		}
		if info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	return latest, nil
}
