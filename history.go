package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"text/tabwriter"
	"time"
)

// Build results, as stored in history.
const (
	resultPassed  = "passed"
	resultFailed  = "failed"
	resultError   = "error"   // checkout or mise install failed
	resultNoTask  = "no-task" // the repo has no task named Task; nothing is posted
	resultDropped = "dropped" // superseded by a newer commit before it ran
)

// Build is one entry in the build history.
type Build struct {
	ID       string    `json:"id,omitempty"` // also the log ID; empty for dropped builds
	Repo     string    `json:"repo"`
	Event    string    `json:"event"` // "push", "pull_request" or "nightly"
	Branch   string    `json:"branch,omitempty"`
	PR       int       `json:"pr,omitempty"`
	SHA      string    `json:"sha,omitempty"`
	Who      string    `json:"who,omitempty"`
	Task     string    `json:"task"`
	Result   string    `json:"result,omitempty"` // empty while queued or running
	Error    string    `json:"error,omitempty"`
	Queued   time.Time `json:"queued,omitzero"` // when the webhook or nightly scan asked for it
	Started  time.Time `json:"started,omitzero"`
	Finished time.Time `json:"finished,omitzero"`
}

func newBuild(j Job) Build {
	return Build{
		Repo:   j.RepoName,
		Event:  j.EventSource,
		Branch: j.Branch,
		PR:     j.PR,
		SHA:    j.CommitSHA,
		Who:    j.TriggeredBy,
		Task:   j.Target,
		Queued: j.Queued,
	}
}

// Trigger describes what caused the build, e.g. "push main" or "pr #4 fix-tz".
func (b Build) Trigger() string {
	switch b.Event {
	case "push":
		return "push " + b.Branch
	case "pull_request":
		return "pr #" + strconv.Itoa(b.PR) + " " + b.Branch
	}
	return b.Event
}

// History is the record of finished builds, one JSON object per line. At
// ~4 MB a year for 50 builds a day it is read whole at startup and kept in
// memory. Only Record changes it, behind this small API, so a database can
// replace the file later without touching callers.
type History struct {
	mu          sync.Mutex
	path        string
	builds      []Build // oldest first
	needNewline bool    // the file ends in a partial line from a crash
}

// history is the server's build history. Nil outside the server, where
// Record does nothing.
var history *History

// openHistory loads the history file at path. A missing file is an empty
// history; unreadable lines are skipped.
func openHistory(path string) (*History, error) {
	h := &History{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return h, nil
	}
	if err != nil {
		return nil, err
	}
	for line := range bytes.Lines(data) {
		var b Build
		if err := json.Unmarshal(line, &b); err != nil {
			slog.Warn("skipping unreadable history line", "path", path, "err", err)
			continue
		}
		h.builds = append(h.builds, b)
	}
	h.needNewline = len(data) > 0 && data[len(data)-1] != '\n'
	return h, nil
}

// Record appends a finished build. A failed write is logged, never returned:
// losing history must not fail a build.
func (h *History) Record(b Build) {
	if h == nil {
		return
	}
	line, err := json.Marshal(b)
	if err != nil {
		slog.Warn("history: encoding build", "err", err)
		return
	}
	line = append(line, '\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	h.builds = append(h.builds, b)
	if h.needNewline {
		// Finish the partial line so this record starts on its own line.
		line = append([]byte{'\n'}, line...)
	}
	if err := appendFile(h.path, line); err != nil {
		slog.Warn("history: writing build", "path", h.path, "err", err)
		return
	}
	h.needNewline = false
}

// Recent returns up to n builds, newest first. A non-empty repo limits them
// to that repo.
func (h *History) Recent(n int, repo string) []Build {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Build
	for i := len(h.builds) - 1; i >= 0 && len(out) < n; i-- {
		if repo == "" || h.builds[i].Repo == repo {
			out = append(out, h.builds[i])
		}
	}
	return out
}

func appendFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// printHistory writes a table of the n most recent builds to w. It backs the
// `punchcard history [repo]` command.
func printHistory(w io.Writer, h *History, logDir string, n int, repo string) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WHEN\tREPO\tTRIGGER\tCOMMIT\tWHO\tRESULT\tWAITED\tTOOK\tLOG")
	for _, b := range h.Recent(n, repo) {
		when, waited, took, log := b.Finished, "-", "-", "-"
		if !b.Started.IsZero() {
			when = b.Started
			took = b.Finished.Sub(b.Started).Round(100 * time.Millisecond).String()
			if !b.Queued.IsZero() { // builds recorded before queued existed have none
				waited = b.Started.Sub(b.Queued).Round(100 * time.Millisecond).String()
			}
		}
		if b.ID != "" {
			log = filepath.Join(logDir, b.ID+".log")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			when.Local().Format("2006-01-02 15:04"), b.Repo, b.Trigger(), shortSHA(b.SHA),
			b.Who, b.Result, waited, took, log)
	}
	return tw.Flush()
}
