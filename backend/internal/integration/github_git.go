package integration

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
)

var ErrTruncatedTree = errors.New("GitHub tree is truncated; snapshot is incomplete")
var objectSHA = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)

// Git database objects follow task quotas rather than the small default used
// for webhooks and issue responses. An explicitly configured HTTP limit remains
// effective; the default imposes no object-size quota.
func (g *GitHubClient) gitAPI(ctx context.Context, method, endpoint string, query url.Values, body, out any) error {
	copy := *g
	if copy.config.MaxResponseBytes <= 0 {
		copy.client.maxBytes = math.MaxInt64 - 1 // request reader adds one byte.
	}
	return copy.api(ctx, method, endpoint, query, body, out)
}

func refPath(ref string) (string, error) {
	ref = strings.TrimPrefix(ref, "refs/")
	if !strings.HasPrefix(ref, "heads/") && !strings.HasPrefix(ref, "tags/") {
		return "", ErrInvalidInput
	}
	if strings.Contains(ref, "..") || strings.ContainsAny(ref, " ~^:?*[\\\x00\r\n") || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") || strings.Contains(ref, "//") || strings.Contains(ref, "@{") {
		return "", ErrInvalidInput
	}
	for _, segment := range strings.Split(ref, "/") {
		if segment == "" || strings.HasPrefix(segment, ".") || strings.HasSuffix(segment, ".lock") {
			return "", ErrInvalidInput
		}
	}
	return ref, nil
}

func (g *GitHubClient) GetRef(ctx context.Context, ref string) (GitRef, error) {
	var out GitRef
	r, err := refPath(ref)
	if err != nil {
		return out, err
	}
	err = g.gitAPI(ctx, http.MethodGet, "/git/ref/"+escapeRef(r), nil, nil, &out)
	if err == nil && (out.Ref != "refs/"+r || !objectSHA.MatchString(out.Object.SHA)) {
		err = &APIError{Provider: "github", Operation: "get_ref", Code: "invalid_identity"}
	}
	return out, err
}
func (g *GitHubClient) GetCommit(ctx context.Context, sha string) (GitCommit, error) {
	var out GitCommit
	if !objectSHA.MatchString(sha) {
		return out, ErrInvalidInput
	}
	err := g.gitAPI(ctx, http.MethodGet, "/git/commits/"+sha, nil, nil, &out)
	if err == nil && (!strings.EqualFold(out.SHA, sha) || !objectSHA.MatchString(out.Tree.SHA)) {
		err = &APIError{Provider: "github", Operation: "get_commit", Code: "invalid_identity"}
	}
	return out, err
}
func (g *GitHubClient) GetTree(ctx context.Context, sha string, recursive bool) (GitTree, error) {
	var out GitTree
	if !objectSHA.MatchString(sha) {
		return out, ErrInvalidInput
	}
	q := url.Values{}
	if recursive {
		q.Set("recursive", "1")
	}
	err := g.gitAPI(ctx, http.MethodGet, "/git/trees/"+sha, q, nil, &out)
	if err == nil && out.Truncated {
		return out, ErrTruncatedTree
	}
	if err == nil && !strings.EqualFold(out.SHA, sha) {
		err = &APIError{Provider: "github", Operation: "get_tree", Code: "invalid_identity"}
	}
	return out, err
}

// GetFullTree falls back to individual directories when GitHub truncates a
// recursive response. No partially returned tree is accepted as a snapshot.
func (g *GitHubClient) GetFullTree(ctx context.Context, sha string) (GitTree, error) {
	tree, err := g.GetTree(ctx, sha, true)
	if !errors.Is(err, ErrTruncatedTree) {
		return tree, err
	}
	result := GitTree{SHA: sha}
	active := map[string]bool{}
	var visit func(string, string) error
	visit = func(current, prefix string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if active[current] {
			return fmt.Errorf("%w: cyclic Git tree", ErrInvalidInput)
		}
		active[current] = true
		defer delete(active, current)
		directory, err := g.GetTree(ctx, current, false)
		if err != nil {
			return err
		}
		for _, entry := range directory.Tree {
			if entry.Path == "" || entry.Path == "." || entry.Path == ".." || strings.ContainsAny(entry.Path, "/\\\x00\r\n") {
				return ErrInvalidInput
			}
			entry.Path = prefix + entry.Path
			result.Tree = append(result.Tree, entry)
			if entry.Type == "tree" {
				if err := visit(entry.SHA, entry.Path+"/"); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(sha, ""); err != nil {
		return GitTree{}, err
	}
	return result, nil
}
func (g *GitHubClient) GetBlob(ctx context.Context, sha string) ([]byte, error) {
	if !objectSHA.MatchString(sha) {
		return nil, ErrInvalidInput
	}
	var out struct {
		SHA      string `json:"sha"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
		Size     int64  `json:"size"`
	}
	err := g.gitAPI(ctx, http.MethodGet, "/git/blobs/"+sha, nil, nil, &out)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(out.SHA, sha) || out.Encoding != "base64" || out.Size < 0 || g.config.MaxResponseBytes > 0 && out.Size > g.config.MaxResponseBytes {
		return nil, &APIError{Provider: "github", Operation: "get_blob", Code: "invalid_blob"}
	}
	data, err := base64.StdEncoding.DecodeString(out.Content)
	if err != nil || int64(len(data)) != out.Size || !strings.EqualFold(blobSHA(data), sha) {
		return nil, &APIError{Provider: "github", Operation: "get_blob", Code: "invalid_blob"}
	}
	return data, nil
}
func blobSHA(data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func (g *GitHubClient) CreateBlob(ctx context.Context, content []byte) (GitObject, error) {
	var out GitObject
	if g.config.MaxResponseBytes > 0 && int64(len(content)) > g.config.MaxResponseBytes {
		return out, ErrInvalidInput
	}
	err := g.gitAPI(ctx, http.MethodPost, "/git/blobs", nil, map[string]string{"content": base64.StdEncoding.EncodeToString(content), "encoding": "base64"}, &out)
	if err == nil && !strings.EqualFold(out.SHA, blobSHA(content)) {
		err = &APIError{Provider: "github", Operation: "create_blob", Code: "invalid_identity", OutcomeUnknown: true}
	}
	return out, err
}
func (g *GitHubClient) CreateTree(ctx context.Context, baseTree string, entries []GitTreeEntry) (GitTree, error) {
	var out GitTree
	if !objectSHA.MatchString(baseTree) || len(entries) == 0 {
		return out, ErrInvalidInput
	}
	tree := make([]map[string]any, 0, len(entries))
	seen := map[string]bool{}
	for _, entry := range entries {
		if entry.Path == "" || entry.Path == "." || path.Clean(entry.Path) != entry.Path || strings.HasPrefix(entry.Path, "/") || strings.HasPrefix(entry.Path, "../") || strings.ContainsAny(entry.Path, "\\\x00\r\n") || seen[entry.Path] {
			return out, ErrInvalidInput
		}
		seen[entry.Path] = true
		if (entry.Type == "blob" && entry.Mode != "100644" && entry.Mode != "100755" && entry.Mode != "120000") || (entry.Type == "tree" && entry.Mode != "040000") || (entry.Type == "commit" && entry.Mode != "160000") || (entry.Type != "blob" && entry.Type != "tree" && entry.Type != "commit") {
			return out, ErrInvalidInput
		}
		item := map[string]any{"path": entry.Path, "mode": entry.Mode, "type": entry.Type}
		if entry.Delete {
			if entry.SHA != "" || entry.Content != nil {
				return out, ErrInvalidInput
			}
			item["sha"] = nil
		} else if entry.Content != nil {
			if entry.SHA != "" || entry.Type != "blob" {
				return out, ErrInvalidInput
			}
			item["content"] = *entry.Content
		} else {
			if !objectSHA.MatchString(entry.SHA) {
				return out, ErrInvalidInput
			}
			item["sha"] = entry.SHA
		}
		tree = append(tree, item)
	}
	err := g.gitAPI(ctx, http.MethodPost, "/git/trees", nil, map[string]any{"base_tree": baseTree, "tree": tree}, &out)
	if err == nil && (!objectSHA.MatchString(out.SHA) || out.Truncated) {
		err = &APIError{Provider: "github", Operation: "create_tree", Code: "invalid_tree", OutcomeUnknown: true}
	}
	return out, err
}
func (g *GitHubClient) CreateCommit(ctx context.Context, message, tree, parent string) (GitCommit, error) {
	var out GitCommit
	if strings.TrimSpace(message) == "" || !objectSHA.MatchString(tree) || !objectSHA.MatchString(parent) {
		return out, ErrInvalidInput
	}
	err := g.gitAPI(ctx, http.MethodPost, "/git/commits", nil, map[string]any{"message": message, "tree": tree, "parents": []string{parent}}, &out)
	if err == nil && (!objectSHA.MatchString(out.SHA) || out.Tree.SHA != tree || len(out.Parents) != 1 || out.Parents[0].SHA != parent) {
		err = &APIError{Provider: "github", Operation: "create_commit", Code: "invalid_identity", OutcomeUnknown: true}
	}
	return out, err
}
func (g *GitHubClient) CreateRef(ctx context.Context, ref, sha string) (GitRef, error) {
	var out GitRef
	r, err := refPath(ref)
	if err != nil || !strings.HasPrefix(r, "heads/") || !objectSHA.MatchString(sha) {
		return out, ErrInvalidInput
	}
	err = g.gitAPI(ctx, http.MethodPost, "/git/refs", nil, map[string]string{"ref": "refs/" + r, "sha": sha}, &out)
	if err == nil && (out.Ref != "refs/"+r || out.Object.SHA != sha) {
		err = &APIError{Provider: "github", Operation: "create_ref", Code: "invalid_identity", OutcomeUnknown: true}
	}
	return out, err
}
func (g *GitHubClient) UpdateRef(ctx context.Context, ref, sha string) (GitRef, error) {
	var out GitRef
	r, err := refPath(ref)
	if err != nil || !strings.HasPrefix(r, "heads/") || !objectSHA.MatchString(sha) {
		return out, ErrInvalidInput
	}
	err = g.gitAPI(ctx, http.MethodPatch, "/git/refs/"+escapeRef(r), nil, map[string]any{"sha": sha, "force": false}, &out)
	if err == nil && (out.Ref != "refs/"+r || out.Object.SHA != sha) {
		err = &APIError{Provider: "github", Operation: "update_ref", Code: "invalid_identity", OutcomeUnknown: true}
	}
	return out, err
}

func escapeRef(ref string) string {
	parts := strings.Split(ref, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

// Marker is a correlation hint, not authorization or a GitHub idempotency guarantee.
func OperationMarker(id string) (string, error) {
	if len(id) < 1 || len(id) > 128 {
		return "", ErrInvalidInput
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return "", ErrInvalidInput
		}
	}
	return "<!-- agentowl-operation:" + id + " -->", nil
}

// LookupPullRequest only reports a match. A missing match cannot prove an ambiguous
// create failed, so callers must keep outcome_unknown instead of automatically recreating.
func (g *GitHubClient) LookupPullRequest(ctx context.Context, head, base, operationID string, maxPages int) (*GitHubPullRequest, error) {
	if head == "" || base == "" || maxPages < 1 || maxPages > 100 {
		return nil, ErrInvalidInput
	}
	marker, err := OperationMarker(operationID)
	if err != nil {
		return nil, err
	}
	for page := 1; page <= maxPages; page++ {
		items, err := g.ListPullRequests(ctx, PullRequestListRequest{State: "all", Head: head, Base: base, Page: page, PerPage: 100})
		if err != nil {
			return nil, err
		}
		var found *GitHubPullRequest
		for _, item := range items {
			if strings.Contains(item.Body, marker) {
				if found != nil {
					return nil, fmt.Errorf("%w: multiple PR matches", ErrInvalidInput)
				}
				copy := item
				found = &copy
			}
		}
		if found != nil {
			return found, nil
		}
		if len(items) < 100 {
			return nil, nil
		}
	}
	return nil, &APIError{Provider: "github", Operation: "lookup_pr", Code: "page_limit_" + strconv.Itoa(maxPages)}
}
