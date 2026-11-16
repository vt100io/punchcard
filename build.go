package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ciTask is the mise task every repo defines; Punchcard runs it on push and PR.
const ciTask = "ci"

type Job struct {
	RepoName    string // "owner/repo"
	CommitSHA   string // empty means the remote's default branch
	Target      string // mise task to run
	TriggeredBy string // GitHub username
	EventSource string // "push", "pull_request", "nightly" or "first"
	Branch      string
	PR          int       // pull request number, 0 if not a PR
	BaseURL     string    // public URL of this server, for log links; empty for nightly and first builds
	Queued      time.Time // set by Schedule

	// done, if set, receives the result when the job finishes.
	done chan<- BuildResult
}

// BuildResult is the outcome of one job.
type BuildResult struct {
	Project  string
	Scanned  bool // false if the repo has no task named Target
	Clean    bool // true only if the task ran and exited 0
	Duration time.Duration
	Output   []string // combined stdout+stderr
	ErrMsg   string
}

// All jobs, webhook and nightly alike, go through this one queue and a single
// worker, so a workspace is never used by two jobs at once.
var queue struct {
	sync.Mutex
	jobs  []Job
	ready chan struct{} // signals the worker that jobs is non-empty
}

func init() { queue.ready = make(chan struct{}, 1) }

// current is the build the worker is running, for live viewers.
var current struct {
	sync.Mutex
	build *Build
}

func setCurrent(b *Build) {
	current.Lock()
	defer current.Unlock()
	if b == nil {
		current.build = nil
		return
	}
	c := *b
	current.build = &c
}

// Snapshot is what the server is doing right now, for live viewers.
type Snapshot struct {
	Running *Build  `json:"running,omitempty"`
	Queue   []Build `json:"queue"`  // oldest first
	Recent  []Build `json:"recent"` // newest first
}

func snapshot(recent int) Snapshot {
	var s Snapshot
	queue.Lock()
	for _, j := range queue.jobs {
		s.Queue = append(s.Queue, newBuild(j))
	}
	queue.Unlock()
	current.Lock()
	if current.build != nil {
		b := *current.build
		s.Running = &b
	}
	current.Unlock()
	s.Recent = history.Recent(recent, "")
	return s
}

// Schedule enqueues a job. Queued jobs for the same branch or PR are dropped:
// only the newest commit is worth building. Dropped jobs never posted a
// status, so they leave no trace on GitHub.
func Schedule(j Job) {
	j.Queued = time.Now()
	queue.Lock()
	if key := j.supersedeKey(); key != "" {
		kept := queue.jobs[:0]
		for _, q := range queue.jobs {
			if q.supersedeKey() == key {
				slog.Info("dropping superseded build", "repo", q.RepoName, "sha", shortSHA(q.CommitSHA), "by", shortSHA(j.CommitSHA))
				b := newBuild(q)
				b.Result = resultDropped
				b.Error = "superseded by " + shortSHA(j.CommitSHA)
				b.Finished = time.Now()
				history.Record(b)
				feed.Publish(Event{Kind: eventDropped, Build: b})
				continue
			}
			kept = append(kept, q)
		}
		queue.jobs = kept
	}
	queue.jobs = append(queue.jobs, j)
	feed.Publish(Event{Kind: eventQueued, Build: newBuild(j)})
	queue.Unlock()

	select {
	case queue.ready <- struct{}{}:
	default: // worker already signalled
	}
}

// nextJob blocks until a job is queued and returns the oldest one.
func nextJob() Job {
	for {
		queue.Lock()
		if len(queue.jobs) > 0 {
			j := queue.jobs[0]
			queue.jobs = queue.jobs[1:]
			queue.Unlock()
			return j
		}
		queue.Unlock()
		<-queue.ready
	}
}

// supersedeKey identifies the line of work a job belongs to: a branch or a
// PR. Nightly jobs return "" and are never dropped.
func (j Job) supersedeKey() string {
	switch j.EventSource {
	case "push":
		return j.RepoName + " branch " + j.Branch
	case "pull_request":
		return j.RepoName + " pr " + strconv.Itoa(j.PR)
	}
	return ""
}

func startWorker(cfg Config, gh *GitHubClient) {
	for {
		j := nextJob()
		r := runJob(cfg, gh, j)
		if j.done != nil {
			j.done <- r
		}
	}
}

// shortSHA returns the first 7 chars of a SHA, or the whole string if shorter.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func runJob(cfg Config, gh *GitHubClient, j Job) BuildResult {
	result := BuildResult{Project: j.RepoName}
	l := slog.With(
		"repo", j.RepoName,
		"who", j.TriggeredBy,
		"event", j.EventSource,
		"branch", j.Branch,
		"sha", shortSHA(j.CommitSHA),
	)

	b := newBuild(j)
	b.ID = newBuildID()
	b.Started = time.Now()
	start := b.Started

	// The build log is written as the build runs, so the link works while
	// it's still in progress. A broken log directory never fails a build.
	pruneLogs(cfg.LogDir())
	targetURL := ""
	var logFile io.Writer = io.Discard
	if f, err := createLog(cfg.LogDir(), b.ID); err != nil {
		l.Warn("build log disabled", "err", err)
	} else {
		defer f.Close()
		logFile = f
		base := j.BaseURL
		if base == "" {
			base = cfg.PublicURL
		}
		if base != "" {
			targetURL = base + "/builds/" + b.ID
		}
	}
	out := io.MultiWriter(logFile, &lineWriter{id: b.ID})

	setCurrent(&b)
	feed.Publish(Event{Kind: eventStarted, Build: b})
	defer func() {
		b.Error = result.ErrMsg
		b.Finished = time.Now()
		history.Record(b)
		setCurrent(nil)
		feed.Publish(Event{Kind: eventFinished, Build: b})
	}()

	fmt.Fprintf(out, "repo:    %s\nevent:   %s\nbranch:  %s\ncommit:  %s\ntask:    %s\nstarted: %s\n\n",
		j.RepoName, j.EventSource, j.Branch, j.CommitSHA, j.Target, start.Format(time.RFC3339))

	// Commit statuses only make sense for a specific commit.
	status := func(state, desc string) {
		if j.CommitSHA != "" {
			setCommitStatus(gh, j.RepoName, j.CommitSHA, state, desc, targetURL)
		}
	}
	finish := func(verdict string) {
		fmt.Fprintf(out, "\n%s after %s\n", verdict, time.Since(start).Truncate(time.Millisecond))
	}
	dir := filepath.Join(cfg.WorkspaceDir(), j.RepoName)
	if err := checkout(l, dir, j, gh); err != nil {
		l.Error("checkout failed", "err", err)
		result.ErrMsg = fmt.Sprintf("checkout failed: %v", err)
		fmt.Fprintln(out, result.ErrMsg)
		finish("ERROR")
		b.Result = resultError
		status("error", "Checkout failed")
		return result
	}

	// Nightly and first builds name no commit. They build the default
	// branch's tip and post its status there, so a repo that breaks without
	// a commit (a new vulnerability, a vanished download) shows it on GitHub.
	if j.CommitSHA == "" {
		if sha, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output(); err == nil {
			j.CommitSHA = strings.TrimSpace(string(sha))
			b.SHA = j.CommitSHA
			fmt.Fprintf(out, "commit:  %s (default branch)\n", j.CommitSHA)
		}
	}
	// At night, a repo without a nightly task runs its ci task instead.
	if j.EventSource == "nightly" && j.Target != ciTask && !hasTask(dir, j) {
		fmt.Fprintf(out, "no %q task, running %q\n", j.Target, ciTask)
		j.Target = ciTask
		b.Task = ciTask
	}

	// A repo opts in by defining the task. Without it, post nothing: the App
	// may be installed on many repos that don't use Punchcard.
	if !hasTask(dir, j) {
		l.Info("skipping: no such mise task", "target", j.Target)
		result.ErrMsg = fmt.Sprintf("no %q task in mise config", j.Target)
		fmt.Fprintln(out, result.ErrMsg)
		finish("SKIPPED")
		b.Result = resultNoTask
		return result
	}
	result.Scanned = true
	status("pending", "Building…")

	if err := installTools(dir, j); err != nil {
		l.Error("mise install failed", "err", err)
		result.ErrMsg = err.Error()
		fmt.Fprintln(out, result.ErrMsg)
		finish("ERROR")
		b.Result = resultError
		status("error", "mise install failed")
		return result
	}

	l.Info("construction started", "target", j.Target, "log", targetURL)
	taskStart := time.Now()
	result.Output, result.ErrMsg = runMise(l, dir, j, out)
	result.Duration = time.Since(taskStart)
	dur := result.Duration.Truncate(time.Millisecond).String()

	if result.ErrMsg != "" {
		l.Error("construction failed", "duration", dur, "err", result.ErrMsg)
		finish("FAILED (" + result.ErrMsg + ")")
		b.Result = resultFailed
		status("failure", "Build failed")
		return result
	}
	result.Clean = true
	l.Info("construction successful", "duration", dur)
	// Publish what the build produced, so a deploy can prove it ships the
	// same bytes: one status per binary, its checksum as the description.
	// Before the success status, so whoever waits for success finds them.
	b.Binaries = binaries(dir, taskStart)
	for name, sum := range b.Binaries {
		fmt.Fprintf(out, "sha256:  %s  %s\n", sum, name)
		if j.CommitSHA != "" {
			postStatus(gh, j.RepoName, j.CommitSHA, "punchcard/sha256 "+name, "success", sum, targetURL)
		}
	}
	finish("PASSED")
	b.Result = resultPassed
	status("success", "Build passed")
	return result
}

// ansiEscape matches terminal escape sequences (colours, cursor moves), which
// tools often emit even without a TTY.
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// runMise runs `mise run <target>`, streaming each output line to the log and
// to out, and capturing it. It returns the lines and a non-empty error
// message on failure.
func runMise(l *slog.Logger, dir string, j Job, out io.Writer) ([]string, string) {
	cmd := mise(dir, j, "run", j.Target)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err.Error()
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return nil, err.Error()
	}

	var lines []string
	scanner := bufio.NewScanner(pipe)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := ansiEscape.ReplaceAllString(scanner.Text(), "")
		lines = append(lines, line)
		fmt.Fprintln(out, line)
		l.Info("➤ " + line)
	}
	if err := scanner.Err(); err != nil {
		l.Warn("output scan error", "err", err)
	}
	if err := cmd.Wait(); err != nil {
		return lines, err.Error()
	}
	return lines, ""
}

// hasTask reports whether the repo defines the job's target task.
func hasTask(dir string, j Job) bool {
	return mise(dir, j, "tasks", "info", j.Target).Run() == nil
}

// mise returns a mise command in dir with the job's environment.
// The PUNCHCARD_* vars let a single "ci" task branch on how it was triggered.
func mise(dir string, j Job, args ...string) *exec.Cmd {
	cmd := exec.Command("mise", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"CI=true",
		"MISE_YES=1",
		"PUNCHCARD_EVENT="+j.EventSource,
		"PUNCHCARD_REPO="+j.RepoName,
		"PUNCHCARD_SHA="+j.CommitSHA,
		"PUNCHCARD_BRANCH="+j.Branch,
	)
	return cmd
}

// setCommitStatus posts a commit status; tests replace it to see what was posted.
var setCommitStatus = (*GitHubClient).SetCommitStatus

// postStatus posts any commit status; tests replace it.
var postStatus = (*GitHubClient).PostStatus

// binaries returns the SHA-256 of each file in dir/bin written since start:
// what this build produced, not what an earlier one left behind.
func binaries(dir string, start time.Time) map[string]string {
	entries, err := os.ReadDir(filepath.Join(dir, "bin"))
	if err != nil {
		return nil
	}
	sums := map[string]string{}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || info.ModTime().Before(start) || len(sums) == 10 {
			continue
		}
		f, err := os.Open(filepath.Join(dir, "bin", e.Name()))
		if err != nil {
			continue
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err == nil {
			sums["bin/"+e.Name()] = hex.EncodeToString(h.Sum(nil))
		}
	}
	if len(sums) == 0 {
		return nil
	}
	return sums
}

// checkout brings dir to the exact commit for j, cloning on first use, and
// trusts its mise config.
func checkout(l *slog.Logger, dir string, j Job, gh *GitHubClient) error {
	url := cloneURL(gh, j.RepoName)

	if _, err := os.Stat(dir); os.IsNotExist(err) {
		l.Info("cloning new workspace")
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return fmt.Errorf("creating workspace parent dir: %w", err)
		}
		if err := git("", "clone", url, dir); err != nil {
			return fmt.Errorf("git clone: %w", err)
		}
	} else {
		// Refresh the remote URL each time so a stale token is never used.
		if err := git(dir, "remote", "set-url", "origin", url); err != nil {
			return fmt.Errorf("updating remote URL: %w", err)
		}
		if err := git(dir, "fetch", "origin"); err != nil {
			return fmt.Errorf("git fetch: %w", err)
		}
	}

	// A fork PR's head commit isn't on any branch in origin.
	if j.PR != 0 {
		if err := git(dir, "fetch", "origin", "pull/"+strconv.Itoa(j.PR)+"/head"); err != nil {
			return fmt.Errorf("fetching PR head: %w", err)
		}
	}

	sha := j.CommitSHA
	if sha == "" {
		sha = "origin/HEAD"
	}
	if err := git(dir, "reset", "--hard", sha); err != nil {
		return fmt.Errorf("git reset: %w", err)
	}
	// Never let state from a previous build leak into this one, except for
	// ignored files (build caches, node_modules, …) which keep builds fast.
	if err := git(dir, "clean", "-fd"); err != nil {
		return fmt.Errorf("git clean: %w", err)
	}

	if err := exec.Command("mise", "trust", dir).Run(); err != nil {
		l.Warn("mise trust failed", "err", err)
	}
	return nil
}

// installTools installs the toolchain pinned in the repo's mise config.
// Near-instant when cached.
func installTools(dir string, j Job) error {
	if out, err := mise(dir, j, "install").CombinedOutput(); err != nil {
		return fmt.Errorf("mise install: %w\n%s", err, ansiEscape.ReplaceAllString(string(out), ""))
	}
	return nil
}

// cloneURL returns an authenticated HTTPS URL when a GitHub App is configured,
// otherwise an SSH URL (local dev, where your SSH agent handles auth).
func cloneURL(gh *GitHubClient, repoName string) string {
	if gh != nil {
		token, err := gh.Token(repoName)
		if err == nil {
			return "https://x-access-token:" + token + "@github.com/" + repoName + ".git"
		}
		slog.Warn("falling back to SSH clone: could not get installation token", "repo", repoName, "err", err)
	}
	return "git@github.com:" + repoName + ".git"
}

func git(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
