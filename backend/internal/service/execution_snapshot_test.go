package service

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestFetchSnapshotConcurrentAndPreservesLimits(t *testing.T) {
	s, a := testService(t)
	const commit = "1111111111111111111111111111111111111111"
	const tree = "2222222222222222222222222222222222222222"
	blobs := map[string][]byte{}
	entries := make([]Data, 12)
	for i := range entries {
		data := []byte(fmt.Sprintf("file content %02d", i))
		hash := sha1.New()
		fmt.Fprintf(hash, "blob %d\x00", len(data))
		_, _ = hash.Write(data)
		sha := hex.EncodeToString(hash.Sum(nil))
		blobs[sha] = data
		entries[i] = Data{"path": fmt.Sprintf("file-%02d.txt", i), "mode": "100644", "type": "blob", "sha": sha, "size": len(data)}
	}
	var active, peak, calls atomic.Int32
	var bad atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/git/ref/heads/main"):
			_ = json.NewEncoder(w).Encode(Data{"ref": "refs/heads/main", "object": Data{"sha": commit}})
		case strings.HasSuffix(r.URL.Path, "/git/commits/"+commit):
			_ = json.NewEncoder(w).Encode(Data{"sha": commit, "tree": Data{"sha": tree}})
		case strings.HasSuffix(r.URL.Path, "/git/trees/"+tree):
			_ = json.NewEncoder(w).Encode(Data{"sha": tree, "tree": entries, "truncated": false})
		case strings.Contains(r.URL.Path, "/git/blobs/"):
			n := active.Add(1)
			defer active.Add(-1)
			calls.Add(1)
			for old := peak.Load(); n > old; old = peak.Load() {
				if peak.CompareAndSwap(old, n) {
					break
				}
			}
			time.Sleep(30 * time.Millisecond)
			sha := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			data, ok := blobs[sha]
			if !ok {
				w.WriteHeader(404)
				return
			}
			if bad.Load() {
				data = []byte("tampered")
			}
			_ = json.NewEncoder(w).Encode(Data{"sha": sha, "encoding": "base64", "content": base64.StdEncoding.EncodeToString(data), "size": len(data)})
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	repo := mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) {
		return s.SaveConfig(context.Background(), tx, a, "repositories", "", Data{"name": "snapshot test", "owner": "owner", "repo": "repo", "base_branch": "main", "enabled": true, "config": Data{"base_url": server.URL}})
	})
	files, ref, treeSHA, err := s.fetchSnapshot(context.Background(), Str(repo, "id"), "")
	if err != nil {
		t.Fatal(err)
	}
	if ref != commit || treeSHA != tree || len(files) != len(entries) {
		t.Fatalf("invalid snapshot: %s %s %d", ref, treeSHA, len(files))
	}
	for i, f := range files {
		if f.Path != Str(entries[i], "path") || string(f.Data) != string(blobs[Str(entries[i], "sha")]) {
			t.Fatalf("snapshot entry %d differs", i)
		}
	}
	if n := peak.Load(); n < 2 || n > 8 {
		t.Fatalf("expected bounded parallel fetch, peak=%d", n)
	}
	bad.Store(true)
	if files, _, _, err = s.fetchSnapshot(context.Background(), Str(repo, "id"), ""); err == nil || files != nil {
		t.Fatal("invalid blob accepted")
	}
	bad.Store(false)
	body := executionSettingsBody(0)
	body["max_files"] = 3
	mutateTest(t, s, a, func(tx pgx.Tx) (Data, error) { return s.SaveExecutionSettings(context.Background(), tx, a, body) })
	before := calls.Load()
	if files, _, _, err = s.fetchSnapshot(context.Background(), Str(repo, "id"), ""); err == nil || files != nil {
		t.Fatal("file quota ignored")
	}
	if calls.Load() != before {
		t.Fatal("downloaded blobs despite known file quota violation")
	}
}
