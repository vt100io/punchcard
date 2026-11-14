package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Config holds all runtime configuration loaded from the environment.
type Config struct {
	Secret      string // webhook HMAC secret
	NightlyTime string // "HH:MM" for the nightly scan, empty to disable
	StateDir    string // root for workspaces/ and reports/
}

func (c Config) WorkspaceDir() string { return filepath.Join(c.StateDir, "workspaces") }
func (c Config) ReportDir() string    { return filepath.Join(c.StateDir, "reports") }
func (c Config) LogDir() string       { return filepath.Join(c.StateDir, "logs") }
func (c Config) HistoryPath() string  { return filepath.Join(c.StateDir, "history.jsonl") }

func main() {
	if len(os.Args) > 1 && os.Args[1] == "tui" {
		if err := tuiCmd(); err != nil {
			fmt.Fprintln(os.Stderr, "punchcard tui:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "history" {
		if err := historyCmd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "punchcard history:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("unable to start Punchcard", "err", err)
		os.Exit(1)
	}
}

// historyCmd prints recent builds: `punchcard history [owner/repo]`.
// It reads the state directory directly, so it works with or without a
// running server.
func historyCmd(args []string) error {
	cfg := Config{StateDir: stateDir()}
	h, err := openHistory(cfg.HistoryPath())
	if err != nil {
		return err
	}
	repo := ""
	if len(args) > 0 {
		repo = args[0]
	}
	return printHistory(os.Stdout, h, cfg.LogDir(), 30, repo)
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	gh, err := newGitHubClient()
	if err != nil {
		return fmt.Errorf("GitHub App config error: %w", err)
	}

	history, err = openHistory(cfg.HistoryPath())
	if err != nil {
		return fmt.Errorf("reading build history: %w", err)
	}

	listener, err := listen()
	if err != nil {
		return fmt.Errorf("failed to set up listener: %w", err)
	}

	control, err := serveControl(cfg)
	if err != nil {
		return fmt.Errorf("opening control socket: %w", err)
	}
	// Close, not Shutdown: /events streams never go idle.
	defer control.Close()
	slog.Info("control socket ready", "path", cfg.SocketPath())

	go startWorker(cfg, gh)
	if cfg.NightlyTime != "" {
		go startNightlyScheduler(cfg)
	}

	mux := http.NewServeMux()
	mux.Handle("GET /builds/{id}", logHandler(cfg))
	mux.Handle("/", webhookHandler(cfg))

	srv := &http.Server{
		Handler:           mux,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ReadHeaderTimeout: 2 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		slog.Info("server listening", "addr", listener.Addr())
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			slog.Error("listen error", "err", err)
		}
	}()

	notifySystemd("READY=1")
	go watchdog()

	<-stop
	slog.Info("shutting down gracefully...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return srv.Shutdown(ctx)
}

// loadConfig builds Config from the environment. Everything else Punchcard
// needs is derived from webhook payloads or from systemd.
func loadConfig() (Config, error) {
	secret := readSecret("PUNCHCARD_SECRET")
	if secret == "" {
		return Config{}, fmt.Errorf("PUNCHCARD_SECRET is not set; refusing to start without a webhook secret")
	}

	cfg := Config{
		Secret:      secret,
		NightlyTime: os.Getenv("PUNCHCARD_NIGHTLY_TIME"),
		StateDir:    stateDir(),
	}
	slog.Info("config loaded", "state_dir", cfg.StateDir, "nightly", cfg.NightlyTime)
	return cfg, nil
}

// stateDir returns $STATE_DIRECTORY (set by systemd's StateDirectory=), or
// ~/.local/state/punchcard when running locally.
func stateDir() string {
	if dir := os.Getenv("STATE_DIRECTORY"); dir != "" {
		// systemd passes a colon-separated list when several are configured.
		return strings.SplitN(dir, ":", 2)[0]
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "punchcard")
	}
	return filepath.Join(home, ".local", "state", "punchcard")
}

// listen returns a listener. Under systemd socket activation it adopts the
// pre-opened socket from fd 3; otherwise it binds the port from $PORT (default 8081).
func listen() (net.Listener, error) {
	if os.Getenv("LISTEN_PID") == strconv.Itoa(os.Getpid()) {
		slog.Info("socket activation detected, adopting fd 3")
		file := os.NewFile(3, "systemd-socket")
		return net.FileListener(file)
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}
	slog.Info("binding directly", "port", port)
	return net.Listen("tcp", ":"+port)
}

// readSecret returns the value of a named secret. It checks
// $CREDENTIALS_DIRECTORY/<name> first (systemd LoadCredential), then
// falls back to the environment variable of the same name.
func readSecret(name string) string {
	if credDir := os.Getenv("CREDENTIALS_DIRECTORY"); credDir != "" {
		data, err := os.ReadFile(filepath.Join(credDir, name))
		if err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	return os.Getenv(name)
}

// notifySystemd sends a state notification to systemd via $NOTIFY_SOCKET.
// No-op when not running under systemd.
func notifySystemd(state string) {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return
	}
	conn, err := net.Dial("unixgram", socket)
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = conn.Write([]byte(state))
}

// watchdog sends WATCHDOG=1 at half the configured interval. No-op locally.
func watchdog() {
	usecStr := os.Getenv("WATCHDOG_USEC")
	if usecStr == "" {
		return
	}
	usec, err := strconv.Atoi(usecStr)
	if err != nil {
		return
	}
	interval := time.Duration(usec) * time.Microsecond / 2
	for {
		notifySystemd("WATCHDOG=1")
		time.Sleep(interval)
	}
}
