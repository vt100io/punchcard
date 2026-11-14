package main

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// The control socket lets local tools such as `punchcard tui` see what the
// server is doing. It is a unix socket readable only by the server's user, so
// it needs no authentication; reach it remotely by SSH-ing to the machine.
//
//	GET /state        the Snapshot as JSON
//	GET /events       the live Feed, one JSON Event per line, until the client hangs up
//	GET /builds/{id}  a build log, as on the public port

func (c Config) SocketPath() string { return filepath.Join(c.StateDir, "punchcard.sock") }

// recentBuilds is how many past builds /state includes.
const recentBuilds = 500

// serveControl listens on the control socket and serves it in the background.
func serveControl(cfg Config) (*http.Server, error) {
	path := cfg.SocketPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// A socket left behind by a server that didn't shut down cleanly.
	// Starting a second server fails earlier, on the TCP port.
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}

	srv := &http.Server{Handler: controlHandler(cfg), ReadHeaderTimeout: 2 * time.Second}
	go func() {
		if err := srv.Serve(l); err != nil && err != http.ErrServerClosed {
			slog.Error("control socket", "err", err)
		}
	}()
	return srv, nil
}

func controlHandler(cfg Config) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(snapshot(recentBuilds))
	})
	mux.HandleFunc("GET /events", eventsHandler)
	mux.Handle("GET /builds/{id}", logHandler(cfg))
	return mux
}

// eventsHandler streams the Feed as JSON lines. A client that reads too
// slowly misses events, so on anything but a log line it should re-read
// /state rather than rely on having seen every event.
func eventsHandler(w http.ResponseWriter, r *http.Request) {
	ch, stop := feed.Subscribe(1024)
	defer stop()

	w.Header().Set("Content-Type", "application/x-ndjson")
	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil { // send headers: the client is now subscribed
		return
	}
	enc := json.NewEncoder(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case e, ok := <-ch:
			if !ok || enc.Encode(e) != nil || rc.Flush() != nil {
				return
			}
		}
	}
}
