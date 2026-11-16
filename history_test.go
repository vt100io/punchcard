package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHistoryRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	h, err := openHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	h.Record(Build{ID: "a", Repo: "o/one", Result: resultPassed})
	h.Record(Build{ID: "b", Repo: "o/two", Result: resultFailed})
	h.Record(Build{ID: "c", Repo: "o/one", Result: resultDropped})

	h2, err := openHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(h2.Recent(10, "")); got != "c b a" {
		t.Errorf("Recent all = %q, want newest first", got)
	}
	if got := ids(h2.Recent(10, "o/one")); got != "c a" {
		t.Errorf("Recent o/one = %q", got)
	}
	if got := ids(h2.Recent(1, "")); got != "c" {
		t.Errorf("Recent 1 = %q", got)
	}
}

// A crash mid-write leaves a partial last line. It is skipped, and the next
// record must not be glued onto it.
func TestHistoryPartialLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	if err := os.WriteFile(path, []byte(`{"id":"a","repo":"o/r"}`+"\n"+`{"id":"b","re`), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := openHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	h.Record(Build{ID: "c", Repo: "o/r"})

	h2, err := openHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(h2.Recent(10, "")); got != "c a" {
		t.Errorf("after partial line, Recent = %q, want %q", got, "c a")
	}
}

func TestNilHistoryRecordIsNoop(t *testing.T) {
	var h *History
	h.Record(Build{ID: "a"}) // must not panic
}

func TestFeedAndLineWriter(t *testing.T) {
	ch, stop := feed.Subscribe(10)
	w := &lineWriter{id: "x"}
	w.Write([]byte("one\ntw"))
	w.Write([]byte("o\nthree")) // "three" is incomplete
	stop()

	var lines []string
	for e := range ch {
		if e.Kind != eventLine || e.Build.ID != "x" || e.N != len(lines) {
			t.Fatalf("unexpected event %+v", e)
		}
		lines = append(lines, e.Line)
	}
	if got := strings.Join(lines, ","); got != "one,two" {
		t.Errorf("lines = %q", got)
	}
	stop() // a second call is harmless
}

func TestFeedNeverBlocks(t *testing.T) {
	_, stop := feed.Subscribe(1) // nobody reads
	defer stop()
	done := make(chan struct{})
	go func() {
		for range 100 {
			feed.Publish(Event{Kind: eventLine})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}
}

func TestScheduleRecordsDroppedBuilds(t *testing.T) {
	useTestHistory(t)
	queue.Lock()
	queue.jobs = nil
	queue.Unlock()
	t.Cleanup(func() {
		queue.Lock()
		queue.jobs = nil
		queue.Unlock()
	})

	ch, stop := feed.Subscribe(10)
	defer stop()
	Schedule(Job{RepoName: "o/r", CommitSHA: "aaaaaaa1", EventSource: "push", Branch: "main", Target: ciTask})
	Schedule(Job{RepoName: "o/r", CommitSHA: "bbbbbbb2", EventSource: "push", Branch: "main", Target: ciTask})

	got := history.Recent(10, "")
	if len(got) != 1 || got[0].SHA != "aaaaaaa1" || got[0].Result != resultDropped || got[0].Error != "superseded by bbbbbbb" {
		t.Fatalf("history = %+v, want one dropped build for aaaaaaa1", got)
	}
	if got[0].Queued.IsZero() || got[0].Finished.Before(got[0].Queued) {
		t.Errorf("dropped build queued %v, finished %v", got[0].Queued, got[0].Finished)
	}
	// The job still queued carries its queued time to the worker.
	if j := nextJob(); j.CommitSHA != "bbbbbbb2" || j.Queued.IsZero() || newBuild(j).Queued != j.Queued {
		t.Errorf("queued job %+v", j)
	}
	if kinds := eventKinds(ch, 3); kinds != "queued dropped queued" {
		t.Errorf("events = %q", kinds)
	}
}

// TestRunJob runs real builds of local repos through git and mise, and
// checks what lands in history, the log, the feed and on GitHub.
func TestRunJob(t *testing.T) {
	if _, err := exec.LookPath("mise"); err != nil {
		t.Skip("mise not installed")
	}

	t.Run("passing build", func(t *testing.T) {
		useTestHistory(t)
		statuses := captureStatuses(t)
		job := localRepo(t, "[tasks.ci]\nrun = \"echo hello from ci\"\n")
		cfg := Config{StateDir: t.TempDir()}
		ch, stop := feed.Subscribe(100)
		defer stop()

		if r := runJob(cfg, nil, job); !r.Clean {
			t.Fatalf("build failed: %s", r.ErrMsg)
		}

		got := history.Recent(10, "")
		if len(got) != 1 {
			t.Fatalf("history has %d builds, want 1", len(got))
		}
		b := got[0]
		if b.Result != resultPassed || b.Repo != "test/app" || b.Who != "stianh" || b.ID == "" || b.Finished.Before(b.Started) {
			t.Errorf("recorded %+v", b)
		}
		log, err := os.ReadFile(filepath.Join(cfg.LogDir(), b.ID+".log"))
		if err != nil || !strings.Contains(string(log), "hello from ci") {
			t.Errorf("log for %s: %v\n%s", b.ID, err, log)
		}
		if got := strings.Join(*statuses, " "); got != "pending success" {
			t.Errorf("statuses = %q, want %q", got, "pending success")
		}

		var sawLine bool
		var kinds []string
		for len(ch) > 0 {
			e := <-ch
			if e.Kind == eventLine {
				sawLine = sawLine || strings.Contains(e.Line, "hello from ci")
				continue
			}
			kinds = append(kinds, e.Kind)
		}
		if strings.Join(kinds, " ") != "started finished" || !sawLine {
			t.Errorf("events = %v, saw ci output line: %v", kinds, sawLine)
		}
	})

	// A passing build publishes the checksum of each binary it wrote, and
	// leaves out what an earlier build left in bin/.
	t.Run("publishes binary checksums", func(t *testing.T) {
		useTestHistory(t)
		captureStatuses(t)
		var posted []string
		old := postStatus
		postStatus = func(_ *GitHubClient, _, _, context, state, desc, _ string) {
			posted = append(posted, context+" "+state+" "+desc)
		}
		t.Cleanup(func() { postStatus = old })

		job := localRepo(t, "[tasks.ci]\nrun = \"mkdir -p bin && printf hello > bin/app\"\n")
		cfg := Config{StateDir: t.TempDir()}
		if r := runJob(cfg, nil, job); !r.Clean {
			t.Fatalf("first build failed: %s", r.ErrMsg)
		}
		// bin/ is ignored by git in real repos, so it survives between
		// builds; here a file an earlier build left behind.
		stale := filepath.Join(cfg.WorkspaceDir(), job.RepoName, "bin", "old")
		writeFile(t, stale, "from an earlier build")
		past := time.Now().Add(-time.Hour)
		if err := os.Chtimes(stale, past, past); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfg.WorkspaceDir(), job.RepoName, ".git", "info", "exclude"), []byte("bin/\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		posted = nil

		if r := runJob(cfg, nil, job); !r.Clean {
			t.Fatalf("build failed: %s", r.ErrMsg)
		}
		// sha256 of "hello"
		want := "punchcard/sha256 bin/app success 2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
		if strings.Join(posted, "|") != want {
			t.Errorf("posted %q, want %q", posted, want)
		}
		if b := history.Recent(1, "")[0]; len(b.Binaries) != 1 || b.Binaries["bin/app"] == "" {
			t.Errorf("history binaries = %v", b.Binaries)
		}
	})

	// At night a repo without a nightly task runs ci, on its default
	// branch's tip, and the status lands on that commit.
	t.Run("nightly runs ci without a nightly task", func(t *testing.T) {
		useTestHistory(t)
		statuses := captureStatuses(t)
		job := localRepo(t, "[tasks.ci]\nrun = \"echo nightly ci\"\n")
		tip := job.CommitSHA
		job.CommitSHA, job.EventSource, job.Target = "", "nightly", "nightly"

		if r := runJob(Config{StateDir: t.TempDir()}, nil, job); !r.Clean {
			t.Fatalf("build failed: %s", r.ErrMsg)
		}
		b := history.Recent(1, "")[0]
		if b.Task != ciTask || b.SHA != tip || b.Result != resultPassed {
			t.Errorf("recorded task %q sha %q result %q, want ci on %s, passed", b.Task, b.SHA, b.Result, tip)
		}
		if got := strings.Join(*statuses, " "); got != "pending success" {
			t.Errorf("statuses = %q, want %q", got, "pending success")
		}
	})

	// A repo without a ci task hasn't opted in: no status is posted, and
	// its tools are not installed.
	t.Run("no ci task", func(t *testing.T) {
		useTestHistory(t)
		statuses := captureStatuses(t)
		job := localRepo(t, "[tools]\nnode = \"17.0.1\"\n\n[tasks.lint]\nrun = \"true\"\n")

		r := runJob(Config{StateDir: t.TempDir()}, nil, job)
		if r.Scanned || r.Clean {
			t.Errorf("result = %+v, want not scanned", r)
		}
		if got := history.Recent(10, ""); len(got) != 1 || got[0].Result != resultNoTask {
			t.Errorf("history = %+v, want one no-task build", got)
		}
		if len(*statuses) != 0 {
			t.Errorf("posted statuses %v, want none", *statuses)
		}
		if strings.Contains(r.ErrMsg, "mise install") {
			t.Errorf("tools were installed: %s", r.ErrMsg)
		}
	})
}

// localRepo creates a git repo with the given mise.toml and makes
// git@github.com:test/app.git resolve to it. It returns a push job for it.
func localRepo(t *testing.T, miseToml string) Job {
	t.Helper()
	origin := t.TempDir()
	repo := filepath.Join(origin, "test", "app.git")
	writeFile(t, filepath.Join(repo, "mise.toml"), miseToml)
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "."},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "init"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	sha, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url."+origin+"/.insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", "git@github.com:")
	return Job{
		RepoName:    "test/app",
		CommitSHA:   strings.TrimSpace(string(sha)),
		EventSource: "push",
		Branch:      "main",
		TriggeredBy: "stianh",
		Target:      ciTask,
	}
}

// captureStatuses records the state of each commit status runJob posts.
func captureStatuses(t *testing.T) *[]string {
	t.Helper()
	var states []string
	old := setCommitStatus
	setCommitStatus = func(_ *GitHubClient, _, _, state, _, _ string) { states = append(states, state) }
	t.Cleanup(func() { setCommitStatus = old })
	return &states
}

func useTestHistory(t *testing.T) {
	t.Helper()
	h, err := openHistory(filepath.Join(t.TempDir(), "history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	old := history
	history = h
	t.Cleanup(func() { history = old })
}

func ids(bs []Build) string {
	var s []string
	for _, b := range bs {
		s = append(s, b.ID)
	}
	return strings.Join(s, " ")
}

func eventKinds(ch <-chan Event, n int) string {
	var s []string
	for range n {
		select {
		case e := <-ch:
			s = append(s, e.Kind)
		case <-time.After(time.Second):
			return strings.Join(s, " ")
		}
	}
	return strings.Join(s, " ")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPrintHistoryShowsWait(t *testing.T) {
	h, err := openHistory(filepath.Join(t.TempDir(), "history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 11, 13, 15, 0, 0, 0, time.UTC)
	b := Build{Event: "push", Branch: "main", SHA: "abcdef1", Who: "stianh", Result: resultPassed, Started: start}
	old, new := b, b
	old.ID, old.Repo, old.Finished = "old", "o/old", start.Add(time.Second)
	new.ID, new.Repo, new.Finished, new.Queued = "new", "o/new", start.Add(4*time.Second), start.Add(-2500*time.Millisecond)
	h.Record(old)
	h.Record(new)

	var out strings.Builder
	if err := printHistory(&out, h, "/logs", 10, ""); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out.String(), "\n")
	if !strings.Contains(lines[0], "WAITED") {
		t.Errorf("header %q lacks WAITED", lines[0])
	}
	// Fields: date, time, repo, "push", branch, commit, who, result, waited, took, log.
	if f := strings.Fields(lines[1]); len(f) != 11 || f[2] != "o/new" || f[8] != "2.5s" || f[9] != "4s" {
		t.Errorf("new build row: %q", lines[1])
	}
	// Builds recorded before queued times existed show no wait.
	if f := strings.Fields(lines[2]); len(f) != 11 || f[2] != "o/old" || f[8] != "-" {
		t.Errorf("old build row: %q", lines[2])
	}
}
