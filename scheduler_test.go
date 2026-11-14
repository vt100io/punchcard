package main

import (
	"path/filepath"
	"testing"
)

// Without an App, the repos Punchcard knows are its workspaces.
func TestKnownReposWithoutApp(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}
	for _, repo := range []string{"vt100io/smed", "vt100io/molt"} {
		writeFile(t, filepath.Join(cfg.WorkspaceDir(), repo, ".git", "HEAD"), "ref: refs/heads/main\n")
	}
	writeFile(t, filepath.Join(cfg.WorkspaceDir(), "vt100io", "not-a-repo", "README"), "")

	got := map[string]bool{}
	for _, r := range knownRepos(cfg, nil) {
		got[r.FullName] = true
	}
	if len(got) != 2 || !got["vt100io/smed"] || !got["vt100io/molt"] {
		t.Errorf("known repos = %v", got)
	}
}
