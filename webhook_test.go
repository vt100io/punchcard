package main

import (
	"errors"
	"testing"
)

// On a public repo anyone can open a PR; only authors with write access get
// a build.
func TestMayBuild(t *testing.T) {
	pr := func(login, association string) PullRequestEvent {
		var ev PullRequestEvent
		ev.Repository.FullName = "vt100io/smed"
		ev.PullRequest.User.Login = login
		ev.PullRequest.AuthorAssociation = association
		return ev
	}

	t.Run("without an App, by association", func(t *testing.T) {
		for association, want := range map[string]bool{
			"OWNER": true, "MEMBER": true, "COLLABORATOR": true,
			"CONTRIBUTOR": false, "FIRST_TIME_CONTRIBUTOR": false, "NONE": false, "": false,
		} {
			if got, _ := mayBuild(nil, pr("someone", association)); got != want {
				t.Errorf("%q: %v, want %v", association, got, want)
			}
		}
	})

	t.Run("with an App, by permission", func(t *testing.T) {
		old := canWrite
		t.Cleanup(func() { canWrite = old })
		canWrite = func(_ *GitHubClient, repo, user string) (bool, error) {
			switch user {
			case "stianh":
				return true, nil
			case "flaky":
				return false, errors.New("GitHub returned 502")
			}
			return false, nil
		}
		gh := &GitHubClient{}
		// The association is ignored: GitHub's answer decides.
		if ok, _ := mayBuild(gh, pr("stianh", "NONE")); !ok {
			t.Error("owner with write access was refused")
		}
		if ok, _ := mayBuild(gh, pr("stranger", "MEMBER")); ok {
			t.Error("author without write access was built")
		}
		if ok, err := mayBuild(gh, pr("flaky", "OWNER")); ok || err == nil {
			t.Error("an API error must mean no build")
		}
	})
}
