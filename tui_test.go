package main

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// The control socket serves /state and streams /events to a real client.
func TestControlSocket(t *testing.T) {
	useTestHistory(t)
	history.Record(Build{ID: "a", Repo: "o/r", Result: resultPassed})

	cfg := Config{StateDir: shortTempDir(t)}
	srv, err := serveControl(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c := newControlClient(cfg.SocketPath())

	resp, err := c.get("/state")
	if err != nil {
		t.Fatal(err)
	}
	var s Snapshot
	err = json.NewDecoder(resp.Body).Decode(&s)
	resp.Body.Close()
	if err != nil || len(s.Recent) != 1 || s.Recent[0].ID != "a" {
		t.Fatalf("state = %+v, err %v", s, err)
	}

	events, err := c.get("/events")
	if err != nil {
		t.Fatal(err)
	}
	defer events.Body.Close()
	// The response headers arrive once the subscription exists, so this
	// event cannot be missed.
	feed.Publish(Event{Kind: eventLine, Build: Build{ID: "a"}, Line: "hello"})

	got := make(chan Event, 1)
	go func() {
		var e Event
		sc := bufio.NewScanner(events.Body)
		if sc.Scan() && json.Unmarshal(sc.Bytes(), &e) == nil {
			got <- e
		}
	}()
	select {
	case e := <-got:
		if e.Kind != eventLine || e.Line != "hello" || e.Build.ID != "a" {
			t.Errorf("event = %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event streamed")
	}
}

func TestRepoRows(t *testing.T) {
	now := time.Now()
	s := Snapshot{
		Running: &Build{ID: "r", Repo: "o/busy", Started: now},
		Queue:   []Build{{Repo: "o/new"}},
		Recent: []Build{
			{ID: "1", Repo: "o/app", Result: resultDropped},
			{ID: "2", Repo: "o/app", Result: resultFailed},
			{ID: "3", Repo: "o/busy", Result: resultPassed},
			{ID: "4", Repo: "o/app", Result: resultPassed},
		},
	}
	var got []string
	for _, r := range repoRows(s) {
		got = append(got, r.name+"="+r.build.ID)
	}
	// Running first; dropped builds don't count as a repo's latest result.
	if want := "o/busy=r o/app=2 o/new="; strings.Join(got, " ") != want {
		t.Errorf("rows = %q, want %q", strings.Join(got, " "), want)
	}
}

// The screen must fill the terminal exactly: no line wider than the
// terminal, and exactly one line per row.
func TestTUILayout(t *testing.T) {
	now := time.Date(2026, 11, 13, 14, 35, 0, 0, time.Local)
	m := newTUI(nil)
	m.now, m.connected = now, true
	m.state = Snapshot{
		Running: &Build{ID: "run", Repo: "vt100io/molt", Event: "push", Branch: "main", SHA: "3f2a1c9aaaa", Started: now.Add(-42 * time.Second)},
		Queue:   []Build{{Repo: "vt100io/punchcard", Event: "pull_request", PR: 12, Branch: "fix", SHA: "a91c0de"}},
		Recent: []Build{
			{ID: "x", Repo: "vt100io/unit", Event: "push", Branch: "main", SHA: "5d01aa2", Result: resultDropped, Error: "superseded by 77be210", Finished: now.Add(-time.Minute)},
			{ID: "p", Repo: "vt100io/punchcard", Event: "push", Branch: "main", Result: resultPassed, Started: now.Add(-2 * time.Minute), Finished: now.Add(-2 * time.Minute)},
			{ID: "f", Repo: "vt100io/smed-with-a-really-long-repository-name", Result: resultFailed, Started: now.Add(-3 * time.Hour), Finished: now.Add(-3 * time.Hour)},
			{ID: "n", Repo: "vt100io/dotfiles", Result: resultNoTask, Started: now.Add(-9 * 24 * time.Hour)},
		},
	}
	m.repos = repoRows(m.state)
	m.selected, m.logID = "vt100io/molt", "run"
	for i := range 80 {
		m.log = append(m.log, "[test] $ go test ./... line "+strings.Repeat("x", i))
	}

	for _, size := range [][2]int{{160, 40}, {120, 30}, {80, 24}, {60, 12}} {
		m.width, m.height = size[0], size[1]
		m.follow = true
		m.clampScroll()
		screen := m.render()
		lines := strings.Split(screen, "\n")
		if len(lines) != m.height {
			t.Errorf("%dx%d: %d lines", m.width, m.height, len(lines))
		}
		for i, l := range lines {
			if w := ansi.StringWidth(l); w > m.width {
				t.Errorf("%dx%d: line %d is %d wide: %q", m.width, m.height, i, w, ansi.Strip(l))
			}
		}
		plain := ansi.Strip(screen)
		for _, want := range []string{"[1] Repos", "[2] Queue", "[3] Log", "worker busy", "1 queued", "vt100io/molt", "running 0:42"} {
			if want == "running 0:42" && m.width < 80 {
				continue // the log title is cut short below 80 columns
			}
			if !strings.Contains(plain, want) {
				t.Errorf("%dx%d: screen lacks %q", m.width, m.height, want)
			}
		}
		// Following: the newest log line is the last one in view.
		if m.offset+m.logHeight() != len(m.log) {
			t.Errorf("%dx%d: offset %d with %d visible doesn't end at line %d", m.width, m.height, m.offset, m.logHeight(), len(m.log))
		}
	}
}

func TestAgo(t *testing.T) {
	now := time.Now()
	for d, want := range map[time.Duration]string{
		10 * time.Second:    "now",
		5 * time.Minute:     "5m",
		3 * time.Hour:       "3h",
		4 * 24 * time.Hour:  "4d",
		21 * 24 * time.Hour: "3w",
	} {
		if got := ago(now, now.Add(-d)); got != want {
			t.Errorf("ago(%v) = %q, want %q", d, got, want)
		}
	}
}

// shortTempDir returns a temp dir with a short path: unix socket paths are
// limited to about 100 bytes, and t.TempDir can exceed that.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// Lines read from the log file and the same lines arriving as live events
// must appear once, in order: the race that duplicated log headers.
func TestTUILogLinesNotDuplicated(t *testing.T) {
	m := newTUI(nil)
	m.width, m.height, m.logID = 120, 30, "b"
	m.Update(logMsg{id: "b", lines: []string{"repo: x", "event: push"}})
	for n, l := range []string{"repo: x", "event: push", "branch: main"} {
		m.Update(eventMsg(Event{Kind: eventLine, Build: Build{ID: "b"}, Line: l, N: n}))
	}
	// A file read that finished after newer events must not drop them.
	m.Update(logMsg{id: "b", lines: []string{"repo: x"}})
	// Lines from another build are ignored.
	m.Update(eventMsg(Event{Kind: eventLine, Build: Build{ID: "other"}, Line: "nope", N: 3}))
	// A gap means events were missed: the log is reloaded, not patched.
	m.Update(eventMsg(Event{Kind: eventLine, Build: Build{ID: "b"}, Line: "far ahead", N: 9}))

	if got := strings.Join(m.log, "|"); got != "repo: x|event: push|branch: main" {
		t.Errorf("log = %q", got)
	}
}

// When the selected repo starts a new build, the log switches to it: the
// scroll position of the old, longer log must not outlive it.
func TestTUISwitchingLogsResetsScroll(t *testing.T) {
	m := newTUI(nil)
	m.width, m.height = 120, 30
	m.Update(stateMsg(Snapshot{Recent: []Build{{ID: "old", Repo: "o/r", Result: resultPassed}}}))
	m.client = nil
	m.Update(logMsg{id: "old", lines: make([]string, 200)})
	if m.offset == 0 {
		t.Fatal("test needs a scrolled log")
	}
	m.Update(stateMsg(Snapshot{Running: &Build{ID: "new", Repo: "o/r"}}))
	_ = m.render() // panicked: slice bounds out of range
	if m.logID != "new" || m.offset != 0 {
		t.Errorf("logID %q offset %d", m.logID, m.offset)
	}
}

func TestStyleLogLineColours(t *testing.T) {
	const red, green = "38;2;239;127;119m", "38;2;124;194;138m" // cBad, cOK as truecolor escapes
	for line, want := range map[string]string{
		"[test] --- FAIL: TestX (0.70s)":    red,
		"[test] FAIL\tgithub.com/x/y\t0.1s": red,
		"FAILED (exit status 1) after 2s":   red,
		"[test] ok  \tgithub.com/x/y\t0.1s": green,
	} {
		if got := styleLogLine(line); !strings.Contains(got, want) {
			t.Errorf("%q is not coloured as expected: %q", line, got)
		}
	}
}
