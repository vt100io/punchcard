package main

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// logRetention is how long build logs are kept.
const logRetention = 30 * 24 * time.Hour

// Build IDs are random, not sequential: logs are served publicly, and an
// unguessable ID means only people who can see the commit can find its log.
var buildID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// newBuildID returns a random build ID, which also names the build's log.
func newBuildID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// createLog creates the log file for build id in dir.
func createLog(dir, id string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return os.Create(filepath.Join(dir, id+".log"))
}

// pruneLogs deletes build logs older than logRetention.
func pruneLogs(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-logRetention)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// logHandler serves GET /builds/{id} as plain text. Reload to follow a
// build that is still running.
func logHandler(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !buildID.MatchString(id) {
			http.NotFound(w, r)
			return
		}
		f, err := os.Open(filepath.Join(cfg.LogDir(), id+".log"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()

		h := w.Header()
		h.Set("Content-Type", "text/plain; charset=utf-8")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Robots-Tag", "noindex")
		h.Set("Cache-Control", "no-store")
		_, _ = io.Copy(w, f)
	}
}

// baseURL returns this server's public URL as seen by the caller, honouring
// X-Forwarded-Proto from a reverse proxy such as Caddy.
func baseURL(r *http.Request) string {
	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" {
		proto = "http"
		if r.TLS != nil {
			proto = "https"
		}
	}
	return proto + "://" + r.Host
}
