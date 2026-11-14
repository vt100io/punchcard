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

func startNightlyScheduler(cfg Config) {
	for {
		next, err := nextOccurrence(cfg.NightlyTime)
		if err != nil {
			slog.Error("invalid nightly time, scheduler stopped", "time", cfg.NightlyTime, "err", err)
			return
		}
		slog.Info("nightly build scheduled", "at", next.Format("2006-01-02 15:04"))
		time.Sleep(time.Until(next))
		runNightlyBuilds(cfg)
	}
}

// runNightlyBuilds runs the "nightly" task in every known workspace. Jobs go
// through the shared queue, so they never overlap with webhook builds.
func runNightlyBuilds(cfg Config) {
	now := time.Now()
	repos := discoverWorkspaces(cfg.WorkspaceDir())
	slog.Info("nightly scan starting", "repos", len(repos))

	done := make(chan BuildResult)
	go func() {
		for _, repo := range repos {
			Schedule(Job{RepoName: repo, Target: "nightly", EventSource: "nightly", done: done})
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
