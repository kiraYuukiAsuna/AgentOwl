package integration

import "time"

type GitHubUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}
type GitHubLabel struct {
	Name string `json:"name"`
}
type GitHubIssue struct {
	ID          int64         `json:"id"`
	Number      int           `json:"number"`
	Title       string        `json:"title"`
	Body        string        `json:"body"`
	State       string        `json:"state"`
	HTMLURL     string        `json:"html_url"`
	User        GitHubUser    `json:"user"`
	Labels      []GitHubLabel `json:"labels"`
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
	PullRequest *struct {
		HTMLURL string `json:"html_url"`
	} `json:"pull_request,omitempty"`
}
type GitHubComment struct {
	ID        int64      `json:"id"`
	Body      string     `json:"body"`
	HTMLURL   string     `json:"html_url"`
	User      GitHubUser `json:"user"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}
type GitHubBranch struct {
	Ref   string `json:"ref"`
	SHA   string `json:"sha"`
	Label string `json:"label"`
}
type GitHubPullRequest struct {
	ID             int64        `json:"id"`
	Number         int          `json:"number"`
	Title          string       `json:"title"`
	Body           string       `json:"body"`
	State          string       `json:"state"`
	HTMLURL        string       `json:"html_url"`
	User           GitHubUser   `json:"user"`
	Head           GitHubBranch `json:"head"`
	Base           GitHubBranch `json:"base"`
	Draft          bool         `json:"draft"`
	Merged         bool         `json:"merged"`
	MergedAt       *time.Time   `json:"merged_at"`
	MergeCommitSHA string       `json:"merge_commit_sha"`
	CreatedAt      time.Time    `json:"created_at"`
	UpdatedAt      time.Time    `json:"updated_at"`
}
type GitHubStatus struct {
	State     string    `json:"state"`
	Context   string    `json:"context"`
	TargetURL string    `json:"target_url"`
	UpdatedAt time.Time `json:"updated_at"`
}
type GitHubCombinedStatus struct {
	State      string         `json:"state"`
	SHA        string         `json:"sha"`
	TotalCount int            `json:"total_count"`
	Statuses   []GitHubStatus `json:"statuses"`
}
type GitHubCheckRun struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	Status     string  `json:"status"`
	Conclusion *string `json:"conclusion"`
	HeadSHA    string  `json:"head_sha"`
	HTMLURL    string  `json:"html_url"`
}
type GitHubCheckRunPage struct {
	TotalCount int              `json:"total_count"`
	CheckRuns  []GitHubCheckRun `json:"check_runs"`
}
type GitHubRelease struct {
	ID          int64      `json:"id"`
	TagName     string     `json:"tag_name"`
	Name        string     `json:"name"`
	Body        string     `json:"body"`
	Draft       bool       `json:"draft"`
	Prerelease  bool       `json:"prerelease"`
	HTMLURL     string     `json:"html_url"`
	PublishedAt *time.Time `json:"published_at"`
}

type IssueListRequest struct {
	State         string
	Since         time.Time
	Page, PerPage int
}
type CreateIssueRequest struct {
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Labels    []string `json:"labels,omitempty"`
	Assignees []string `json:"assignees,omitempty"`
}
type UpdateIssueRequest struct {
	Title     *string   `json:"title,omitempty"`
	Body      *string   `json:"body,omitempty"`
	State     *string   `json:"state,omitempty"`
	Labels    *[]string `json:"labels,omitempty"`
	Assignees *[]string `json:"assignees,omitempty"`
}
type PullRequestListRequest struct {
	State, Head, Base string
	Page, PerPage     int
}
type CreatePullRequestRequest struct {
	Title               string `json:"title"`
	Body                string `json:"body"`
	Head                string `json:"head"`
	Base                string `json:"base"`
	Draft               bool   `json:"draft"`
	MaintainerCanModify bool   `json:"maintainer_can_modify"`
}
type UpdatePullRequestRequest struct {
	Title *string `json:"title,omitempty"`
	Body  *string `json:"body,omitempty"`
	State *string `json:"state,omitempty"`
}

type GitObject struct {
	SHA  string `json:"sha"`
	Type string `json:"type,omitempty"`
}
type GitRef struct {
	Ref    string    `json:"ref"`
	Object GitObject `json:"object"`
}
type GitCommit struct {
	SHA     string      `json:"sha"`
	Message string      `json:"message"`
	Tree    GitObject   `json:"tree"`
	Parents []GitObject `json:"parents"`
}
type GitTreeEntry struct {
	Path    string  `json:"path"`
	Mode    string  `json:"mode"`
	Type    string  `json:"type"`
	SHA     string  `json:"sha,omitempty"`
	Size    int64   `json:"size,omitempty"`
	Content *string `json:"content,omitempty"`
	Delete  bool    `json:"-"`
}
type GitTree struct {
	SHA       string         `json:"sha"`
	Tree      []GitTreeEntry `json:"tree"`
	Truncated bool           `json:"truncated"`
}
