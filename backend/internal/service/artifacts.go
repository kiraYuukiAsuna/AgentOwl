package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentowl/backend/internal/runner"
	"github.com/jackc/pgx/v5"
)

const artifactStorageLock int64 = 728690106

func artifactError(message string) error { return Fail(422, "invalid_artifact", message) }

func (s *Service) artifactRoot() (*os.Root, error) {
	root := filepath.Join(s.Config.DataDir, "artifacts")
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	return os.OpenRoot(root)
}

// The transaction lock spans both disk reservation and DB commit across workers.
// A rollback after rename leaves an orphan; cleanup waits for this same lock.
func (s *Service) storeArtifact(ctx context.Context, tx pgx.Tx, taskID, caseID, runID, name, mediaType string, body []byte) (Data, error) {
	if name == "" || len(name) > 255 || strings.ContainsAny(name, "/\\\x00\r\n") {
		return nil, artifactError("产物名称无效")
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", artifactStorageLock); err != nil {
		return nil, err
	}
	root, err := s.artifactRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id := ID()
	temporary, relative := ".tmp-"+id, id+".blob"
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	defer root.Remove(temporary)
	_, err = file.Write(body)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = root.Rename(temporary, relative); err != nil {
		return nil, err
	}
	directory, err := root.Open(".")
	if err == nil {
		err = directory.Sync()
		_ = directory.Close()
	}
	if err != nil {
		_ = root.Remove(relative)
		return nil, err
	}
	result, err := one(ctx, tx, `INSERT INTO artifacts(id,task_id,case_id,run_id,name,relative_path,sha256,size,media_type) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING to_jsonb(artifacts)`, id, taskID, caseID, runID, name, relative, Hash(body), len(body), mediaType)
	if err != nil {
		_ = root.Remove(relative)
		return nil, err
	}
	return result, nil
}

func artifactEntries(root *os.Root) ([]os.FileInfo, error) {
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	return directory.Readdir(-1)
}

func validArtifactPath(name string) bool {
	return strings.HasSuffix(name, ".blob") && len(name) == 41 && validID(strings.TrimSuffix(name, ".blob")) && filepath.Base(name) == name
}

// ReadArtifactContent verifies the stored bytes. HTTP callers must first enforce
// the artifact's case/repository ACL through Get; this method is also used by workers.
func (s *Service) ReadArtifactContent(ctx context.Context, id string) ([]byte, error) {
	return s.readArtifact(ctx, id)
}

func (s *Service) readArtifact(ctx context.Context, id string) ([]byte, error) {
	if !validID(id) {
		return nil, NotFound
	}
	row, err := one(ctx, s.DB, "SELECT to_jsonb(a) FROM artifacts a WHERE id=$1 AND status='ready'", id)
	if err != nil {
		return nil, err
	}
	relative := Str(row, "relative_path")
	if !validArtifactPath(relative) || Num(row, "size") < 0 {
		return nil, artifactError("产物元信息无效")
	}
	root, err := s.artifactRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	info, err := root.Lstat(relative)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != Num(row, "size") {
		return nil, artifactError("产物不是普通文件")
	}
	file, err := root.Open(relative)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, Num(row, "size")))
	if err != nil {
		return nil, err
	}
	var extra [1]byte
	n, endErr := file.Read(extra[:])
	if int64(len(body)) != Num(row, "size") || n != 0 || endErr != io.EOF || Hash(body) != Str(row, "sha256") {
		return nil, artifactError("产物完整性校验失败")
	}
	return body, ctx.Err()
}

func (s *Service) storeSnapshot(ctx context.Context, tx pgx.Tx, taskID, caseID, runID string, files []runner.File) (Data, error) {
	if _, err := runner.SnapshotDigest(files); err != nil {
		return nil, err
	}
	body, err := json.Marshal(files)
	if err != nil {
		return nil, err
	}
	return s.storeArtifact(ctx, tx, taskID, caseID, runID, "snapshot.json", "application/json", body)
}

func (s *Service) readSnapshot(ctx context.Context, id string) ([]runner.File, error) {
	body, err := s.readArtifact(ctx, id)
	if err != nil {
		return nil, err
	}
	var files []runner.File
	if err = decodeArtifactJSON(body, &files); err != nil {
		return nil, err
	}
	if _, err = runner.SnapshotDigest(files); err != nil {
		return nil, err
	}
	return files, nil
}

// CleanupOrphanArtifacts only removes old files with no committed DB reference.
// The grace period also preserves files for manual investigation after a failure.
func (s *Service) CleanupOrphanArtifacts(ctx context.Context) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", artifactStorageLock); err != nil {
		return err
	}
	root, err := s.artifactRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	entries, err := artifactEntries(root)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-time.Hour)
	for _, entry := range entries {
		if !entry.Mode().IsRegular() || !entry.ModTime().Before(cutoff) {
			continue
		}
		name := entry.Name()
		temporary := strings.HasPrefix(name, ".tmp-") && validID(strings.TrimPrefix(name, ".tmp-"))
		if !temporary && !validArtifactPath(name) {
			continue
		}
		var referenced bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM artifacts WHERE relative_path=$1)", name).Scan(&referenced); err != nil {
			return err
		}
		if !referenced {
			if err = root.Remove(name); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

type snapshotManifestEntry struct {
	Path   string `json:"path"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func decodeArtifactJSON(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return artifactError("产物包含多余 JSON 内容")
	}
	return nil
}

func decodeRunArchive(reader io.Reader, expected runner.RunSpec) (runner.Run, []runner.File, string, error) {
	run, files, diff, _, err := decodeRunArchiveWithLogs(reader, expected)
	return run, files, diff, err
}

func decodeRunArchiveWithLogs(reader io.Reader, expected runner.RunSpec) (runner.Run, []runner.File, string, map[string][]byte, error) {
	fail := func(err error) (runner.Run, []runner.File, string, map[string][]byte, error) {
		return runner.Run{}, nil, "", nil, err
	}
	if expected.Limits.ArtifactBytes < 0 || expected.Limits.WorkspaceBytes < 0 || expected.Limits.MaxFiles < 0 {
		return fail(artifactError("执行产物限制不能为负数"))
	}
	gz, err := gzip.NewReader(reader)
	if err != nil {
		return fail(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	entries := map[string]runner.File{}
	var total, workspaceBytes int64
	var files []runner.File
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fail(err)
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return fail(artifactError("运行产物不允许目录、链接或设备"))
		}
		if header.Mode != 0644 && header.Mode != 0755 {
			return fail(artifactError("运行产物文件权限无效"))
		}
		if _, found := entries[header.Name]; found {
			return fail(artifactError("运行产物存在重复路径"))
		}
		if header.Size < 0 || header.Size > math.MaxInt64-total || expected.Limits.ArtifactBytes > 0 && header.Size > expected.Limits.ArtifactBytes-total {
			return fail(artifactError("运行产物大小超限"))
		}
		workspace := strings.HasPrefix(header.Name, "workspace/")
		if !workspace && !contains([]string{"result.json", "manifest.json", "diff.patch", "tools.ndjson", "test.log"}, header.Name) {
			return fail(artifactError("运行产物包含未声明路径"))
		}
		if workspace && (expected.Limits.WorkspaceBytes > 0 && header.Size > expected.Limits.WorkspaceBytes-workspaceBytes || expected.Limits.MaxFiles > 0 && len(files) >= expected.Limits.MaxFiles) {
			return fail(artifactError("工作区产物超限"))
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return fail(err)
		}
		if int64(len(body)) != header.Size {
			return fail(io.ErrUnexpectedEOF)
		}
		total += header.Size
		entry := runner.File{Path: header.Name, Mode: uint32(header.Mode), Data: body}
		entries[header.Name] = entry
		if workspace {
			entry.Path = strings.TrimPrefix(header.Name, "workspace/")
			files = append(files, entry)
			workspaceBytes += header.Size
			if expected.Limits.WorkspaceBytes > 0 && workspaceBytes > expected.Limits.WorkspaceBytes || expected.Limits.MaxFiles > 0 && len(files) > expected.Limits.MaxFiles {
				return fail(artifactError("工作区产物超限"))
			}
		}
	}
	// Read the gzip checksum and reject hidden data after tar's end marker.
	var remainder [4096]byte
	for {
		n, err := gz.Read(remainder[:])
		for _, b := range remainder[:n] {
			if b != 0 {
				return fail(artifactError("归档结束标记后存在额外内容"))
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fail(err)
		}
	}
	for _, name := range []string{"result.json", "manifest.json", "diff.patch"} {
		if _, ok := entries[name]; !ok {
			return fail(artifactError("运行产物缺少必要文件"))
		}
	}
	var result runner.Run
	if err = decodeArtifactJSON(entries["result.json"].Data, &result); err != nil {
		return fail(err)
	}
	digest, err := runner.SpecDigest(expected)
	if err != nil {
		return fail(err)
	}
	if result.RunID != expected.RunID || result.TaskID != expected.TaskID || result.AttemptID != expected.AttemptID || result.Epoch != expected.Epoch || result.Stage != expected.Stage || result.SpecDigest != digest || result.Result.InputDigest != expected.InputDigest || result.Result.TreeSHA != expected.TreeSHA {
		return fail(artifactError("运行产物身份或输入证据不匹配"))
	}
	if !contains([]string{"succeeded", "failed", "stopped", "blocked", "outcome_unknown"}, result.State) {
		return fail(artifactError("运行产物尚未结束"))
	}
	outputDigest, err := runner.SnapshotDigest(files)
	if err != nil {
		return fail(err)
	}
	if outputDigest != result.Result.OutputDigest {
		return fail(artifactError("运行产物快照摘要不匹配"))
	}
	var manifest []snapshotManifestEntry
	if err = decodeArtifactJSON(entries["manifest.json"].Data, &manifest); err != nil {
		return fail(err)
	}
	if len(manifest) != len(files) {
		return fail(artifactError("运行产物清单与文件数量不一致"))
	}
	seen := map[string]bool{}
	for _, item := range manifest {
		file, exists := entries["workspace/"+item.Path]
		if !exists || seen[item.Path] || item.Size != int64(len(file.Data)) || item.Mode != file.Mode || item.SHA256 != Hash(file.Data) {
			return fail(artifactError("运行产物清单校验失败"))
		}
		seen[item.Path] = true
	}
	logs := map[string][]byte{}
	for _, name := range []string{"test.log", "tools.ndjson"} {
		if entry, ok := entries[name]; ok {
			logs[name] = entry.Data
		}
	}
	return result, files, string(entries["diff.patch"].Data), logs, nil
}
