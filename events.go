package main

// Repository is the subset of GitHub's repository object Punchcard uses.
type Repository struct {
	FullName      string `json:"full_name"` // "owner/repo"
	DefaultBranch string `json:"default_branch"`
}

type PushEvent struct {
	Ref        string     `json:"ref"`     // e.g. refs/heads/main
	After      string     `json:"after"`   // the commit SHA
	Deleted    bool       `json:"deleted"` // true when the branch was deleted
	Repository Repository `json:"repository"`
	Sender     struct {
		Login string `json:"login"`
	} `json:"sender"`
}

type PullRequestEvent struct {
	Action      string     `json:"action"`
	Number      int        `json:"number"`
	Repository  Repository `json:"repository"` // the base repo, even for fork PRs
	PullRequest struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		AuthorAssociation string `json:"author_association"` // OWNER, MEMBER, COLLABORATOR, CONTRIBUTOR, NONE, …
		Head              struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	} `json:"pull_request"`
}
