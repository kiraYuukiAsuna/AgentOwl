package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentowl/backend/internal/runner"
	"github.com/jackc/pgx/v5"
)

type archiveTestEntry struct {
	name string
	data []byte
	mode int64
	kind byte
}

func artifactFixture(t *testing.T) (runner.RunSpec, []archiveTestEntry) {
	t.Helper()
	files := []runner.File{{Path: "internal/auth.go", Mode: 0644, Data: []byte("package auth\n")}, {Path: "scripts/test", Mode: 0755, Data: []byte("#!/bin/sh\nexit 0\n")}}
	digest, err := runner.SnapshotDigest(files)
	if err != nil {
		t.Fatal(err)
	}
	spec := runner.RunSpec{RunID: "run-1", TaskID: "task-1", StepID: "step-1", AttemptID: "attempt-1", Epoch: 2, Stage: "edit", InputDigest: digest, TreeSHA: strings.Repeat("a", 40), Files: files, Limits: runner.Limits{MaxFiles: 10, WorkspaceBytes: 1 << 20, ArtifactBytes: 2 << 20}}
	specDigest, _ := runner.SpecDigest(spec)
	result := runner.Run{RunID: spec.RunID, TaskID: spec.TaskID, AttemptID: spec.AttemptID, Epoch: spec.Epoch, Stage: spec.Stage, State: "succeeded", SpecDigest: specDigest, Result: runner.Result{InputDigest: digest, OutputDigest: digest, TreeSHA: spec.TreeSHA, EvidenceValid: true}}
	manifest := []snapshotManifestEntry{}
	entries := []archiveTestEntry{{name: "result.json", data: JSON(result)}, {name: "manifest.json"}, {name: "diff.patch", data: []byte("verified diff")}}
	for _, file := range files {
		manifest = append(manifest, snapshotManifestEntry{Path: file.Path, Mode: file.Mode, Size: int64(len(file.Data)), SHA256: Hash(file.Data)})
		entries = append(entries, archiveTestEntry{name: "workspace/" + file.Path, mode: int64(file.Mode), data: file.Data})
	}
	entries[1].data = JSON(manifest)
	return spec, entries
}

func artifactArchive(t *testing.T, entries []archiveTestEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		mode := entry.mode
		if mode == 0 {
			mode = 0644
		}
		kind := entry.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		header := &tar.Header{Name: entry.name, Size: int64(len(entry.data)), Mode: mode, Typeflag: kind}
		if kind == tar.TypeSymlink {
			header.Linkname = "../../outside"
			header.Size = 0
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if kind == tar.TypeReg {
			if _, err := tw.Write(entry.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestDecodeRunArchiveVerifiesEvidence(t *testing.T) {
	spec, entries := artifactFixture(t)
	result, files, diff, err := decodeRunArchive(bytes.NewReader(artifactArchive(t, entries)), spec)
	if err != nil {
		t.Fatal(err)
	}
	if result.RunID != spec.RunID || !reflect.DeepEqual(files, spec.Files) || diff != "verified diff" {
		t.Fatal("verified archive did not preserve evidence")
	}
	if result.ResourceLimits != "" || result.ResourceWarning != "" {
		t.Fatal("archive result without resource metadata was changed")
	}
}

func TestDecodeRunArchivePreservesDisabledResourceLimits(t *testing.T) {
	spec, entries := artifactFixture(t)
	var wireResult map[string]json.RawMessage
	if err := json.Unmarshal(entries[0].data, &wireResult); err != nil {
		t.Fatal(err)
	}
	warning := "development mode: CPU, memory and PID hard limits are disabled; tasks may exhaust host resources"
	wireResult["resource_limits"] = json.RawMessage(`"disabled"`)
	wireResult["resource_warning"] = JSON(warning)
	entries[0].data = JSON(wireResult)

	result, files, diff, err := decodeRunArchive(bytes.NewReader(artifactArchive(t, entries)), spec)
	if err != nil {
		t.Fatal(err)
	}
	if result.ResourceLimits != runner.ResourceLimitsDisabled || result.ResourceWarning != warning {
		t.Fatalf("resource metadata was not preserved: mode=%q warning=%q", result.ResourceLimits, result.ResourceWarning)
	}
	if result.RunID != spec.RunID || !result.Result.EvidenceValid || !reflect.DeepEqual(files, spec.Files) || diff != "verified diff" {
		t.Fatal("resource metadata changed validated archive evidence")
	}
}

func TestDecodeRunArchivePreservesValidatedLogs(t *testing.T) {
	spec, entries := artifactFixture(t)
	entries = append(entries, archiveTestEntry{name: "test.log", data: []byte("all tests passed\n")}, archiveTestEntry{name: "tools.ndjson", data: []byte("{\"tool\":\"read_file\"}\n")})
	_, _, _, logs, err := decodeRunArchiveWithLogs(bytes.NewReader(artifactArchive(t, entries)), spec)
	if err != nil {
		t.Fatal(err)
	}
	if string(logs["test.log"]) != "all tests passed\n" || string(logs["tools.ndjson"]) != "{\"tool\":\"read_file\"}\n" || len(logs) != 2 {
		t.Fatal("validated execution logs not preserved")
	}
}

func TestArtifactStorageHasNoFixedAggregateQuota(t *testing.T) {
	s, _ := testService(t)
	root, err := s.artifactRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	file, err := root.OpenFile(ID()+".blob", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate(6 << 30); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	_ = file.Close()
	err = s.transaction(context.Background(), func(tx pgx.Tx) error {
		_, err := s.storeArtifact(context.Background(), tx, "", "", "", "test.log", "text/plain", []byte("x"))
		return err
	})
	if err != nil {
		t.Fatalf("an unrelated sparse file imposed a fixed storage quota: %v", err)
	}
}

func TestDecodeRunArchiveRejectsTampering(t *testing.T) {
	cases := map[string]func(*runner.RunSpec, *[]archiveTestEntry){
		"wrong run": func(_ *runner.RunSpec, e *[]archiveTestEntry) {
			var run runner.Run
			_ = json.Unmarshal((*e)[0].data, &run)
			run.RunID = "other"
			(*e)[0].data = JSON(run)
		},
		"wrong attempt": func(_ *runner.RunSpec, e *[]archiveTestEntry) {
			var run runner.Run
			_ = json.Unmarshal((*e)[0].data, &run)
			run.AttemptID = "other"
			(*e)[0].data = JSON(run)
		},
		"stale spec": func(s *runner.RunSpec, _ *[]archiveTestEntry) { s.Goal = "changed goal" },
		"manifest digest": func(_ *runner.RunSpec, e *[]archiveTestEntry) {
			var m []snapshotManifestEntry
			_ = json.Unmarshal((*e)[1].data, &m)
			m[0].SHA256 = strings.Repeat("0", 64)
			(*e)[1].data = JSON(m)
		},
		"manifest size": func(_ *runner.RunSpec, e *[]archiveTestEntry) {
			var m []snapshotManifestEntry
			_ = json.Unmarshal((*e)[1].data, &m)
			m[0].Size++
			(*e)[1].data = JSON(m)
		},
		"manifest duplicate": func(_ *runner.RunSpec, e *[]archiveTestEntry) {
			var m []snapshotManifestEntry
			_ = json.Unmarshal((*e)[1].data, &m)
			m[1] = m[0]
			(*e)[1].data = JSON(m)
		},
		"changed bytes":   func(_ *runner.RunSpec, e *[]archiveTestEntry) { (*e)[3].data = []byte("corrupted") },
		"path traversal":  func(_ *runner.RunSpec, e *[]archiveTestEntry) { (*e)[3].name = "workspace/../escape" },
		"absolute path":   func(_ *runner.RunSpec, e *[]archiveTestEntry) { (*e)[3].name = "/outside" },
		"symlink":         func(_ *runner.RunSpec, e *[]archiveTestEntry) { (*e)[3].kind = tar.TypeSymlink },
		"special mode":    func(_ *runner.RunSpec, e *[]archiveTestEntry) { (*e)[3].mode = 04755 },
		"duplicate entry": func(_ *runner.RunSpec, e *[]archiveTestEntry) { *e = append(*e, (*e)[3]) },
		"undeclared file": func(_ *runner.RunSpec, e *[]archiveTestEntry) {
			*e = append(*e, archiveTestEntry{name: "credentials.json", data: []byte("{}")})
		},
		"missing manifest": func(_ *runner.RunSpec, e *[]archiveTestEntry) { *e = append((*e)[:1], (*e)[2:]...) },
		"byte quota":       func(s *runner.RunSpec, _ *[]archiveTestEntry) { s.Limits.ArtifactBytes = 1 },
		"file quota":       func(s *runner.RunSpec, _ *[]archiveTestEntry) { s.Limits.MaxFiles = 1 },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			spec, entries := artifactFixture(t)
			change(&spec, &entries)
			if _, _, _, err := decodeRunArchive(bytes.NewReader(artifactArchive(t, entries)), spec); err == nil {
				t.Fatal("untrusted archive accepted")
			}
		})
	}
	t.Run("gzip checksum", func(t *testing.T) {
		spec, entries := artifactFixture(t)
		archive := artifactArchive(t, entries)
		archive[len(archive)-8] ^= 1
		if _, _, _, err := decodeRunArchive(bytes.NewReader(archive), spec); err == nil {
			t.Fatal("corrupt gzip accepted")
		}
	})
}

func TestArtifactStorageIntegrityAndRollbackCleanup(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	files := []runner.File{{Path: "auth.go", Mode: 0644, Data: []byte("package auth\n")}}
	var stored Data
	if err := s.transaction(ctx, func(tx pgx.Tx) error {
		var err error
		stored, err = s.storeSnapshot(ctx, tx, ID(), ID(), ID(), files)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	actual, err := s.readSnapshot(ctx, Str(stored, "id"))
	if err != nil || !reflect.DeepEqual(actual, files) {
		t.Fatalf("snapshot roundtrip: %v", err)
	}
	path := filepath.Join(s.Config.DataDir, "artifacts", Str(stored, "relative_path"))
	if err = os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.readArtifact(ctx, Str(stored, "id")); err == nil {
		t.Fatal("corrupt stored artifact accepted")
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err = os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, err = s.readArtifact(ctx, Str(stored, "id")); err == nil {
		t.Fatal("symlink accepted")
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := s.storeArtifact(ctx, tx, "", "", "", "orphan.txt", "text/plain", []byte("orphan"))
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	orphanPath := filepath.Join(s.Config.DataDir, "artifacts", Str(orphan, "relative_path"))
	old := time.Now().Add(-2 * time.Hour)
	if err = os.Chtimes(orphanPath, old, old); err != nil {
		t.Fatal(err)
	}
	if err = s.CleanupOrphanArtifacts(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Fatalf("rollback orphan was not removed: %v", err)
	}
}
