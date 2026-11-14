package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// webhookHandler accepts push and pull_request events. Anyone who can make
// GitHub deliver a signed webhook is trusted to run the repo's ci task: install
// Punchcard only on repos where you trust everyone who can open a PR.
func webhookHandler(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		if !verifyGitHubSignature(body, r.Header.Get("X-Hub-Signature-256"), cfg.Secret) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}

		event := r.Header.Get("X-GitHub-Event")

		switch event {
		case "push":
			var ev PushEvent
			if err := json.Unmarshal(body, &ev); err != nil {
				slog.Error("invalid push payload", "err", err)
				http.Error(w, "bad payload", http.StatusBadRequest)
				return
			}
			branch := strings.TrimPrefix(ev.Ref, "refs/heads/")
			if ev.Deleted || branch != ev.Repository.DefaultBranch {
				break
			}

			slog.Info("inbound push",
				"repo", ev.Repository.FullName,
				"branch", branch,
				"who", ev.Sender.Login,
				"sha", shortSHA(ev.After),
			)
			Schedule(Job{
				RepoName:    ev.Repository.FullName,
				CommitSHA:   ev.After,
				Target:      ciTask,
				TriggeredBy: ev.Sender.Login,
				EventSource: "push",
				Branch:      branch,
				BaseURL:     baseURL(r),
			})

		case "pull_request":
			var ev PullRequestEvent
			if err := json.Unmarshal(body, &ev); err != nil {
				slog.Error("invalid PR payload", "err", err)
				http.Error(w, "bad payload", http.StatusBadRequest)
				return
			}
			if ev.Action != "opened" && ev.Action != "synchronize" && ev.Action != "reopened" {
				break
			}

			slog.Info("inbound pull request",
				"repo", ev.Repository.FullName,
				"pr", ev.Number,
				"action", ev.Action,
				"who", ev.PullRequest.User.Login,
				"flow", fmt.Sprintf("%s -> %s", ev.PullRequest.Head.Ref, ev.PullRequest.Base.Ref),
			)
			// Always build in the base repo: its workspace, its commit status.
			// Fork PRs are fetched via refs/pull/N/head.
			Schedule(Job{
				RepoName:    ev.Repository.FullName,
				CommitSHA:   ev.PullRequest.Head.SHA,
				Target:      ciTask,
				TriggeredBy: ev.PullRequest.User.Login,
				EventSource: "pull_request",
				Branch:      ev.PullRequest.Head.Ref,
				PR:          ev.Number,
				BaseURL:     baseURL(r),
			})

		default:
			slog.Debug("received unhandled event", "event", event)
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}

func verifyGitHubSignature(body []byte, signature string, secret string) bool {
	if signature == "" {
		return false
	}
	const prefix = "sha256="
	if !strings.HasPrefix(signature, prefix) {
		return false
	}
	expectedMAC, err := hex.DecodeString(signature[len(prefix):])
	if err != nil {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), expectedMAC)
}
