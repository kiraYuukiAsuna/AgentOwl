package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGitDatabaseObjectsDoNotUseIssueResponseQuota(t *testing.T) {
	content := bytes.Repeat([]byte("large"), (5<<20)/5+1)
	sha := blobSHA(content)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("Git database request lost authentication")
		}
		if request.Method == http.MethodPost {
			var input map[string]string
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			got, err := base64.StdEncoding.DecodeString(input["content"])
			if err != nil || !bytes.Equal(got, content) {
				t.Error("large blob write changed content")
			}
			writeJSON(t, w, GitObject{SHA: sha})
			return
		}
		// A client with an explicit small limit intentionally closes this body.
		_ = json.NewEncoder(w).Encode(map[string]any{"sha": sha, "encoding": "base64", "content": base64.StdEncoding.EncodeToString(content), "size": len(content)})
	}))
	defer server.Close()
	cfg := GitHubConfig{BaseURL: server.URL, Owner: "owner", Repo: "repo", Token: "test-token", HTTPClient: server.Client()}
	client := mustGitHub(t, cfg)
	got, err := client.GetBlob(context.Background(), sha)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatal("large Git blob hit the default issue-response limit", err)
	}
	if _, err = client.CreateBlob(context.Background(), content); err != nil {
		t.Fatal("large Git blob could not be published", err)
	}
	if client.client.maxBytes != 4<<20 {
		t.Fatal("Git database override changed webhook/issue client limits")
	}
	cfg.MaxResponseBytes = 1024
	limited := mustGitHub(t, cfg)
	if _, err = limited.GetBlob(context.Background(), sha); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatal("explicit transport limit was ignored", err)
	}
}

func TestGetFullTreeFallsBackWithoutAcceptingTruncation(t *testing.T) {
	root, child := strings.Repeat("a", 40), strings.Repeat("b", 40)
	for _, scenario := range []string{"complete", "cycle", "truncated_directory"} {
		t.Run(scenario, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				sha := strings.TrimPrefix(request.URL.Path, "/repos/owner/repo/git/trees/")
				if request.URL.Query().Get("recursive") == "1" {
					writeJSON(t, w, GitTree{SHA: sha, Truncated: true})
					return
				}
				if sha == root {
					writeJSON(t, w, GitTree{SHA: root, Tree: []GitTreeEntry{{Path: "src", Type: "tree", Mode: "040000", SHA: child}}})
					return
				}
				tree := GitTree{SHA: child, Tree: []GitTreeEntry{{Path: "file.go", Type: "blob", Mode: "100644", SHA: blobSHA([]byte("source")), Size: 6}}}
				if scenario == "cycle" {
					tree.Tree = []GitTreeEntry{{Path: "back", Type: "tree", Mode: "040000", SHA: root}}
				}
				tree.Truncated = scenario == "truncated_directory"
				writeJSON(t, w, tree)
			}))
			defer server.Close()
			client := mustGitHub(t, GitHubConfig{BaseURL: server.URL, Owner: "owner", Repo: "repo", HTTPClient: server.Client()})
			tree, err := client.GetFullTree(context.Background(), root)
			if scenario != "complete" {
				if err == nil {
					t.Fatal("incomplete or cyclic tree was accepted")
				}
				return
			}
			if err != nil || tree.Truncated || len(tree.Tree) != 2 || tree.Tree[1].Path != "src/file.go" {
				t.Fatal("directory traversal did not reconstruct the complete tree", tree, err)
			}
		})
	}
}
