package integration

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type GitHubConfig struct {
	BaseURL, Token, WebhookSecret, Owner, Repo string
	HTTPClient                                 HTTPDoer
	Timeout                                    time.Duration
	MaxResponseBytes                           int64
	AllowHTTP                                  bool
}
type GitHubClient struct {
	client   client
	config   GitHubConfig
	repoPath string
}

var repositorySegment = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func NewGitHubClient(cfg GitHubConfig) (*GitHubClient, error) {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.github.com"
	}
	if !repositorySegment.MatchString(cfg.Owner) || !repositorySegment.MatchString(cfg.Repo) || cfg.Owner == "." || cfg.Owner == ".." || cfg.Repo == "." || cfg.Repo == ".." {
		return nil, fmt.Errorf("%w: repository owner/name required", ErrInvalidInput)
	}
	c, err := newClient("github", cfg.BaseURL, cfg.HTTPClient, cfg.Timeout, cfg.MaxResponseBytes, cfg.AllowHTTP)
	if err != nil {
		return nil, err
	}
	return &GitHubClient{client: c, config: cfg, repoPath: "/repos/" + cfg.Owner + "/" + cfg.Repo}, nil
}

type GitHubEvent struct {
	DeliveryID string
	// BodySHA256 is signed-body identity; GitHub delivery headers are not signed.
	BodySHA256               string
	Kind, Action, Repository string
	Event                    *Event
	Issue                    *GitHubIssue
	Comment                  *GitHubComment
	PullRequest              *GitHubPullRequest
	Ignored                  bool
}

func (g *GitHubClient) ParseWebhook(headers http.Header, body []byte) (GitHubEvent, error) {
	if g.config.WebhookSecret == "" || len(body) == 0 || int64(len(body)) > g.client.maxBytes {
		return GitHubEvent{}, ErrInvalidWebhook
	}
	signature := headers.Get("X-Hub-Signature-256")
	if !strings.HasPrefix(signature, "sha256=") {
		return GitHubEvent{}, ErrInvalidWebhook
	}
	provided, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if err != nil {
		return GitHubEvent{}, ErrInvalidWebhook
	}
	mac := hmac.New(sha256.New, []byte(g.config.WebhookSecret))
	mac.Write(body)
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return GitHubEvent{}, ErrInvalidWebhook
	}
	if _, err = decodeUniqueJSON(body); err != nil {
		return GitHubEvent{}, ErrInvalidWebhook
	}
	var payload struct {
		Action     string `json:"action"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Sender      GitHubUser         `json:"sender"`
		Issue       *GitHubIssue       `json:"issue"`
		Comment     *GitHubComment     `json:"comment"`
		PullRequest *GitHubPullRequest `json:"pull_request"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return GitHubEvent{}, ErrInvalidWebhook
	}
	kind := headers.Get("X-GitHub-Event")
	delivery := headers.Get("X-GitHub-Delivery")
	if delivery == "" || kind == "" {
		return GitHubEvent{}, ErrInvalidWebhook
	}
	if !strings.EqualFold(payload.Repository.FullName, g.config.Owner+"/"+g.config.Repo) {
		return GitHubEvent{}, ErrInvalidWebhook
	}
	digest := sha256.Sum256(body)
	hash := hex.EncodeToString(digest[:])
	out := GitHubEvent{DeliveryID: delivery, BodySHA256: hash, Kind: kind, Action: payload.Action, Repository: payload.Repository.FullName}
	event := Event{ID: "github:" + hash, Kind: "github." + kind, Action: payload.Action, Repository: payload.Repository.FullName, ActorID: strconv.FormatInt(payload.Sender.ID, 10), Bot: payload.Sender.Type == "Bot"}
	switch kind {
	case "issues":
		if payload.Issue == nil || payload.Issue.Number <= 0 || payload.Issue.ID <= 0 || payload.Action == "" {
			return GitHubEvent{}, ErrInvalidWebhook
		}
		issue := payload.Issue
		out.Issue = issue
		event.ChannelID = "issue:" + strconv.Itoa(issue.Number)
		event.MessageID = "issue:" + strconv.FormatInt(issue.ID, 10)
		event.Number = issue.Number
		event.Text = Redact(issue.Title + "\n\n" + issue.Body)
		event.SourceURL = issue.HTMLURL
		event.SourceRevision = issue.UpdatedAt.Format(time.RFC3339Nano)
		event.OccurredAt = issue.UpdatedAt
		event.Deleted = payload.Action == "deleted"
	case "issue_comment":
		if payload.Issue == nil || payload.Comment == nil || payload.Comment.ID <= 0 || payload.Issue.Number <= 0 || payload.Action == "" {
			return GitHubEvent{}, ErrInvalidWebhook
		}
		comment := payload.Comment
		out.Issue = payload.Issue
		out.Comment = comment
		event.ChannelID = "issue:" + strconv.Itoa(payload.Issue.Number)
		event.MessageID = "comment:" + strconv.FormatInt(comment.ID, 10)
		event.ParentID = "issue:" + strconv.FormatInt(payload.Issue.ID, 10)
		event.Number = payload.Issue.Number
		event.Text = Redact(comment.Body)
		event.SourceURL = comment.HTMLURL
		event.SourceRevision = comment.UpdatedAt.Format(time.RFC3339Nano)
		event.OccurredAt = comment.UpdatedAt
		event.Deleted = payload.Action == "deleted"
	case "pull_request":
		if payload.PullRequest == nil || payload.PullRequest.Number <= 0 || payload.PullRequest.ID <= 0 || payload.Action == "" {
			return GitHubEvent{}, ErrInvalidWebhook
		}
		pr := payload.PullRequest
		out.PullRequest = pr
		event.ChannelID = "pull:" + strconv.Itoa(pr.Number)
		event.MessageID = "pull:" + strconv.FormatInt(pr.ID, 10)
		event.Number = pr.Number
		event.Text = Redact(pr.Title + "\n\n" + pr.Body)
		event.SourceURL = pr.HTMLURL
		event.SourceRevision = pr.UpdatedAt.Format(time.RFC3339Nano)
		event.OccurredAt = pr.UpdatedAt
	default:
		out.Ignored = true
		return out, nil
	}
	if payload.Sender.ID <= 0 || event.OccurredAt.IsZero() {
		return GitHubEvent{}, ErrInvalidWebhook
	}
	// A deletion may keep the last content updated_at; it still needs its own version.
	if event.Deleted {
		event.SourceRevision += ":deleted:" + hash
	}
	out.Event = &event
	return out, nil
}

func (g *GitHubClient) api(ctx context.Context, method, path string, q url.Values, body, out any) error {
	headers := http.Header{"Accept": {"application/vnd.github+json"}, "X-GitHub-Api-Version": {"2022-11-28"}, "User-Agent": {"AgentOwl"}}
	if g.config.Token != "" {
		headers.Set("Authorization", "Bearer "+g.config.Token)
	}
	data, _, err := g.client.request(ctx, method, g.repoPath+path, q, headers, body)
	if err != nil {
		return err
	}
	if out != nil {
		return decodeResponse("github", method, data, out)
	}
	return nil
}

func pageQuery(page, perPage int) (url.Values, error) {
	if page == 0 {
		page = 1
	}
	if perPage == 0 {
		perPage = 100
	}
	if page < 1 || perPage < 1 || perPage > 100 {
		return nil, ErrInvalidInput
	}
	return url.Values{"page": {strconv.Itoa(page)}, "per_page": {strconv.Itoa(perPage)}}, nil
}
func validState(s string) bool { return s == "open" || s == "closed" || s == "all" }

type GitHubRepository struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
}

func (g *GitHubClient) GetRepository(ctx context.Context) (GitHubRepository, error) {
	var out GitHubRepository
	e := g.api(ctx, http.MethodGet, "", nil, nil, &out)
	if e == nil && (out.ID <= 0 || !strings.EqualFold(out.FullName, g.config.Owner+"/"+g.config.Repo)) {
		e = &APIError{Provider: "github", Operation: "repository", Code: "invalid_identity"}
	}
	return out, e
}

type GitHubReview struct {
	ID          int64      `json:"id"`
	State       string     `json:"state"`
	User        GitHubUser `json:"user"`
	CommitID    string     `json:"commit_id"`
	SubmittedAt *time.Time `json:"submitted_at"`
	HTMLURL     string     `json:"html_url"`
}

func (g *GitHubClient) ListPullRequestReviews(ctx context.Context, number, page int) ([]GitHubReview, error) {
	if number <= 0 {
		return nil, ErrInvalidInput
	}
	q, e := pageQuery(page, 100)
	if e != nil {
		return nil, e
	}
	var out []GitHubReview
	e = g.api(ctx, http.MethodGet, "/pulls/"+strconv.Itoa(number)+"/reviews", q, nil, &out)
	return out, e
}

// AuthenticatedUser works for user/PAT tokens. Installation tokens may reject
// this endpoint; callers must not infer an app's posting identity from that.
func (g *GitHubClient) AuthenticatedUser(ctx context.Context) (GitHubUser, error) {
	var out GitHubUser
	h := http.Header{"Accept": {"application/vnd.github+json"}, "X-GitHub-Api-Version": {"2022-11-28"}, "User-Agent": {"AgentOwl"}}
	if g.config.Token != "" {
		h.Set("Authorization", "Bearer "+g.config.Token)
	}
	b, _, e := g.client.request(ctx, http.MethodGet, "/user", nil, h, nil)
	if e != nil {
		return out, e
	}
	if e = decodeResponse("github", "authenticated_user", b, &out); e != nil {
		return out, e
	}
	if out.ID <= 0 || out.Login == "" {
		return out, &APIError{Provider: "github", Operation: "authenticated_user", Code: "invalid_identity"}
	}
	return out, nil
}

func (g *GitHubClient) GetIssue(ctx context.Context, number int) (GitHubIssue, error) {
	var out GitHubIssue
	if number <= 0 {
		return out, ErrInvalidInput
	}
	err := g.api(ctx, http.MethodGet, "/issues/"+strconv.Itoa(number), nil, nil, &out)
	if err == nil && (out.Number != number || out.ID <= 0) {
		err = &APIError{Provider: "github", Operation: "get_issue", Code: "invalid_identity"}
	}
	return out, err
}
func (g *GitHubClient) ListIssues(ctx context.Context, req IssueListRequest) ([]GitHubIssue, error) {
	q, err := pageQuery(req.Page, req.PerPage)
	if err != nil {
		return nil, err
	}
	if req.State == "" {
		req.State = "all"
	}
	if !validState(req.State) {
		return nil, ErrInvalidInput
	}
	q.Set("state", req.State)
	q.Set("sort", "updated")
	q.Set("direction", "asc")
	if !req.Since.IsZero() {
		q.Set("since", req.Since.UTC().Format(time.RFC3339))
	}
	var out []GitHubIssue
	err = g.api(ctx, http.MethodGet, "/issues", q, nil, &out)
	return out, err
}
func (g *GitHubClient) CreateIssue(ctx context.Context, req CreateIssueRequest) (GitHubIssue, error) {
	var out GitHubIssue
	if strings.TrimSpace(req.Title) == "" {
		return out, ErrInvalidInput
	}
	err := g.api(ctx, http.MethodPost, "/issues", nil, req, &out)
	if err == nil && out.Number <= 0 {
		err = &APIError{Provider: "github", Operation: "create_issue", Code: "missing_identity", OutcomeUnknown: true}
	}
	return out, err
}
func (g *GitHubClient) UpdateIssue(ctx context.Context, number int, req UpdateIssueRequest) (GitHubIssue, error) {
	var out GitHubIssue
	if number <= 0 || (req.State != nil && *req.State != "open" && *req.State != "closed") {
		return out, ErrInvalidInput
	}
	err := g.api(ctx, http.MethodPatch, "/issues/"+strconv.Itoa(number), nil, req, &out)
	if err == nil && (out.Number != number || out.ID <= 0) {
		err = &APIError{Provider: "github", Operation: "update_issue", Code: "invalid_identity", OutcomeUnknown: true}
	}
	return out, err
}
func (g *GitHubClient) CreateComment(ctx context.Context, number int, body string) (GitHubComment, error) {
	var out GitHubComment
	if number <= 0 || strings.TrimSpace(body) == "" {
		return out, ErrInvalidInput
	}
	err := g.api(ctx, http.MethodPost, "/issues/"+strconv.Itoa(number)+"/comments", nil, map[string]string{"body": body}, &out)
	if err == nil && out.ID <= 0 {
		err = &APIError{Provider: "github", Operation: "create_comment", Code: "missing_identity", OutcomeUnknown: true}
	}
	return out, err
}
func (g *GitHubClient) ListComments(ctx context.Context, number, page int) ([]GitHubComment, error) {
	return g.ListCommentsSince(ctx, number, page, time.Time{})
}
func (g *GitHubClient) ListCommentsSince(ctx context.Context, number, page int, since time.Time) ([]GitHubComment, error) {
	if number <= 0 {
		return nil, ErrInvalidInput
	}
	q, err := pageQuery(page, 100)
	if err != nil {
		return nil, err
	}
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}
	var out []GitHubComment
	err = g.api(ctx, http.MethodGet, "/issues/"+strconv.Itoa(number)+"/comments", q, nil, &out)
	return out, err
}
func (g *GitHubClient) GetPullRequest(ctx context.Context, number int) (GitHubPullRequest, error) {
	var out GitHubPullRequest
	if number <= 0 {
		return out, ErrInvalidInput
	}
	err := g.api(ctx, http.MethodGet, "/pulls/"+strconv.Itoa(number), nil, nil, &out)
	if err == nil && (out.Number != number || out.ID <= 0) {
		err = &APIError{Provider: "github", Operation: "get_pr", Code: "invalid_identity"}
	}
	return out, err
}
func (g *GitHubClient) ListPullRequests(ctx context.Context, req PullRequestListRequest) ([]GitHubPullRequest, error) {
	q, err := pageQuery(req.Page, req.PerPage)
	if err != nil {
		return nil, err
	}
	if req.State == "" {
		req.State = "all"
	}
	if !validState(req.State) {
		return nil, ErrInvalidInput
	}
	q.Set("state", req.State)
	q.Set("sort", "updated")
	q.Set("direction", "desc")
	if req.Head != "" {
		q.Set("head", req.Head)
	}
	if req.Base != "" {
		q.Set("base", req.Base)
	}
	var out []GitHubPullRequest
	err = g.api(ctx, http.MethodGet, "/pulls", q, nil, &out)
	return out, err
}
func (g *GitHubClient) CreatePullRequest(ctx context.Context, req CreatePullRequestRequest) (GitHubPullRequest, error) {
	var out GitHubPullRequest
	if req.Head == "" || req.Base == "" || strings.TrimSpace(req.Title) == "" {
		return out, ErrInvalidInput
	}
	err := g.api(ctx, http.MethodPost, "/pulls", nil, req, &out)
	if err == nil && out.Number <= 0 {
		err = &APIError{Provider: "github", Operation: "create_pr", Code: "missing_identity", OutcomeUnknown: true}
	}
	return out, err
}
func (g *GitHubClient) UpdatePullRequest(ctx context.Context, number int, req UpdatePullRequestRequest) (GitHubPullRequest, error) {
	var out GitHubPullRequest
	if number <= 0 || (req.State != nil && *req.State != "open" && *req.State != "closed") {
		return out, ErrInvalidInput
	}
	err := g.api(ctx, http.MethodPatch, "/pulls/"+strconv.Itoa(number), nil, req, &out)
	if err == nil && (out.Number != number || out.ID <= 0) {
		err = &APIError{Provider: "github", Operation: "update_pr", Code: "invalid_identity", OutcomeUnknown: true}
	}
	return out, err
}
func (g *GitHubClient) GetCombinedStatus(ctx context.Context, ref string) (GitHubCombinedStatus, error) {
	var out GitHubCombinedStatus
	if ref == "" {
		return out, ErrInvalidInput
	}
	err := g.api(ctx, http.MethodGet, "/commits/"+url.PathEscape(ref)+"/status", nil, nil, &out)
	if err == nil && (out.SHA == "" || (out.State != "success" && out.State != "pending" && out.State != "failure")) {
		err = &APIError{Provider: "github", Operation: "get_status", Code: "invalid_status"}
	}
	return out, err
}
func (g *GitHubClient) ListCheckRuns(ctx context.Context, ref string, page int) (GitHubCheckRunPage, error) {
	var out GitHubCheckRunPage
	if ref == "" {
		return out, ErrInvalidInput
	}
	q, err := pageQuery(page, 100)
	if err != nil {
		return out, err
	}
	q.Set("filter", "latest")
	err = g.api(ctx, http.MethodGet, "/commits/"+url.PathEscape(ref)+"/check-runs", q, nil, &out)
	return out, err
}
func (g *GitHubClient) ListReleases(ctx context.Context, page int) ([]GitHubRelease, error) {
	q, err := pageQuery(page, 100)
	if err != nil {
		return nil, err
	}
	var out []GitHubRelease
	err = g.api(ctx, http.MethodGet, "/releases", q, nil, &out)
	return out, err
}
