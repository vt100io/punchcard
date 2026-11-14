package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func startNightlyScheduler(cfg Config, gh *GitHubClient) {
	for {
		next, err := nextOccurrence(cfg.NightlyTime)
		if err != nil {
			slog.Error("invalid nightly time, scheduler stopped", "time", cfg.NightlyTime, "err", err)
			return
		}
		slog.Info("nightly build scheduled", "at", next.Format("2006-01-02 15:04"))
		time.Sleep(time.Until(next))
		runNightlyBuilds(cfg, gh)
	}
}

// runNightlyBuilds builds every repo the App is installed on, and every
// workspace, whether anyone pushed today or not: its "nightly" task, or "ci"
// without one. Jobs go through the shared queue, so they never overlap with
// webhook builds.
func runNightlyBuilds(cfg Config, gh *GitHubClient) {
	now := time.Now()
	repos := knownRepos(cfg, gh)
	slog.Info("nightly scan starting", "repos", len(repos))

	done := make(chan BuildResult)
	go func() {
		for _, r := range repos {
			Schedule(Job{RepoName: r.FullName, Branch: r.DefaultBranch, Target: "nightly", EventSource: "nightly", done: done})
		}
	}()

	var results []BuildResult
	for range repos {
		r := <-done
		results = append(results, r)
		switch {
		case !r.Scanned:
			slog.Warn("nightly: not scanned", "repo", r.Project, "reason", r.ErrMsg)
		case r.Clean:
			slog.Info("nightly: clean", "repo", r.Project, "duration", r.Duration.Truncate(time.Millisecond))
		default:
			slog.Warn("nightly: flagged", "repo", r.Project, "err", r.ErrMsg)
		}
	}

	path := filepath.Join(cfg.ReportDir(), "nightly-"+now.Format("2006-01-02")+".txt")
	if err := writeReport(results, now, path); err != nil {
		slog.Error("failed to write nightly report", "err", err)
	} else {
		slog.Info("nightly report written", "path", path)
	}
}

// buildNewRepos gives every repo the App can see but Punchcard hasn't built
// yet a first build of its default branch, so installing the App is enough:
// no push needed. Repos without a ci task are cloned, skipped, and not
// tried again.
func buildNewRepos(cfg Config, gh *GitHubClient) {
	repos, err := gh.Repos()
	if err != nil {
		slog.Warn("could not list the App's repos", "err", err)
		return
	}
	for _, r := range repos {
		if _, err := os.Stat(filepath.Join(cfg.WorkspaceDir(), r.FullName)); os.IsNotExist(err) {
			slog.Info("new repo, first build", "repo", r.FullName)
			Schedule(Job{RepoName: r.FullName, Branch: r.DefaultBranch, Target: ciTask, EventSource: "first"})
		}
	}
}

// knownRepos is every repo the App can see plus every workspace, once each.
// Without an App, it is the workspaces.
func knownRepos(cfg Config, gh *GitHubClient) []AppRepo {
	repos, err := gh.Repos()
	if err != nil {
		slog.Warn("could not list the App's repos; using workspaces only", "err", err)
	}
	seen := map[string]bool{}
	for _, r := range repos {
		seen[r.FullName] = true
	}
	for _, name := range discoverWorkspaces(cfg.WorkspaceDir()) {
		if !seen[name] {
			seen[name] = true
			repos = append(repos, AppRepo{FullName: name})
		}
	}
	return repos
}

// discoverWorkspaces returns all "owner/repo" names found under base.
// Only directories with a .git subdirectory are included.
func discoverWorkspaces(base string) []string {
	var repos []string
	owners, err := os.ReadDir(base)
	if err != nil {
		slog.Warn("could not read workspace dir", "path", base, "err", err)
		return nil
	}
	for _, owner := range owners {
		if !owner.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(base, owner.Name()))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			if _, err := os.Stat(filepath.Join(base, owner.Name(), entry.Name(), ".git")); err == nil {
				repos = append(repos, owner.Name()+"/"+entry.Name())
			}
		}
	}
	return repos
}

// nextOccurrence returns the next wall-clock moment matching "HH:MM" in local time.
func nextOccurrence(timeStr string) (time.Time, error) {
	parts := strings.SplitN(timeStr, ":", 2)
	if len(parts) != 2 {
		return time.Time{}, fmt.Errorf("expected HH:MM, got %q", timeStr)
	}
	hour, err := strconv.Atoi(parts[0])
	if err != nil || hour < 0 || hour > 23 {
		return time.Time{}, fmt.Errorf("invalid hour in %q", timeStr)
	}
	min, err := strconv.Atoi(parts[1])
	if err != nil || min < 0 || min > 59 {
		return time.Time{}, fmt.Errorf("invalid minute in %q", timeStr)
	}

	now := time.Now()
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, now.Location())
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next, nil
}
