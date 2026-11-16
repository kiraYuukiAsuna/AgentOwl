package runner

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

func validPath(name string) error {
	if name == "" || name == "." || len(name) > 1024 || strings.ContainsAny(name, "\\\x00\r\n") || path.IsAbs(name) || path.Clean(name) != name {
		return errors.New("invalid relative file path")
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." || part == ".git" || strings.Contains(part, ":") {
			return errors.New("forbidden file path")
		}
	}
	return nil
}

func pathAllowed(name string, allowed []string) bool {
	for _, p := range allowed {
		if p == "." || name == p || strings.HasPrefix(name, strings.TrimSuffix(p, "/")+"/") {
			return true
		}
	}
	return false
}

func safeJoin(root, name string) (string, error) {
	if err := validPath(name); err != nil {
		return "", err
	}
	current := root
	for _, part := range strings.Split(name, "/") {
		current = filepath.Join(current, part)
		st, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("symbolic links are not supported")
		}
	}
	return current, nil
}

func storeSnapshot(root string, files []File) error {
	if err := os.MkdirAll(root, 0755); err != nil {
		return err
	}
	for _, f := range files {
		name, err := safeJoin(root, f.Path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			return err
		}
		fd, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(f.Mode))
		if err != nil {
			return err
		}
		_, err = fd.Write(f.Data)
		closeErr := fd.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func readSnapshot(root string, maxBytes int64, maxFiles int) ([]File, error) {
	if maxBytes < 0 || maxFiles < 0 {
		return nil, errors.New("invalid workspace quota")
	}
	var files []File
	var total int64
	err := filepath.WalkDir(root, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == root {
			return nil
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if err := validPath(rel); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("symbolic link in workspace")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("non-regular workspace file")
		}
		if info.Size() < 0 || maxBytes > 0 && (total > maxBytes || info.Size() > maxBytes-total) || maxFiles > 0 && len(files) >= maxFiles {
			return errors.New("workspace quota exceeded")
		}
		file, err := os.Open(name)
		if err != nil {
			return err
		}
		var reader io.Reader = file
		if maxBytes > 0 {
			reader = io.LimitReader(file, addSize(maxBytes-total, 1))
		}
		data, readErr := io.ReadAll(reader)
		closeErr := file.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		total = addSize(total, int64(len(data)))
		if maxBytes > 0 && total > maxBytes {
			return errors.New("workspace quota exceeded")
		}
		mode := uint32(0644)
		if info.Mode()&0111 != 0 {
			mode = 0755
		}
		files = append(files, File{Path: rel, Mode: mode, Data: data})
		return nil
	})
	return files, err
}

func fileTar(name string, mode uint32, data []byte) ([]byte, error) {
	if err := validPath(name); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	if err := w.WriteHeader(&tar.Header{Name: name, Mode: int64(mode), Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		return nil, err
	}
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// readArchive rejects links and devices before exposing Docker archive contents.
func readArchive(reader io.Reader, maxBytes int64, maxFiles int) ([]File, error) {
	if maxBytes < 0 || maxFiles < 0 {
		return nil, errors.New("invalid archive quota")
	}
	if maxBytes > 0 && maxFiles > 0 {
		reader = io.LimitReader(reader, addSize(addSize(maxBytes, multiplySize(int64(maxFiles), 2048)), 1))
	}
	tr := tar.NewReader(reader)
	var files []File
	seen := map[string]bool{}
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(h.Name, "/")
		if err := validPath(name); err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, errors.New("duplicate archive entry")
		}
		seen[name] = true
		if maxFiles > 0 && len(seen) > maxFiles {
			return nil, errors.New("archive inode limit exceeded")
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
			return nil, errors.New("archive links and devices are forbidden")
		}
		if h.Size < 0 || maxBytes > 0 && (total > maxBytes || h.Size > maxBytes-total) {
			return nil, errors.New("archive byte limit exceeded")
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		if int64(len(data)) != h.Size {
			return nil, io.ErrUnexpectedEOF
		}
		total = addSize(total, h.Size)
		mode := uint32(0644)
		if h.Mode&0111 != 0 {
			mode = 0755
		}
		files = append(files, File{Path: name, Mode: mode, Data: data})
	}
}

func diffText(before, after []File) string {
	old := map[string]File{}
	fresh := map[string]File{}
	for _, f := range before {
		old[f.Path] = f
	}
	for _, f := range after {
		fresh[f.Path] = f
	}
	keys := map[string]bool{}
	for k := range old {
		keys[k] = true
	}
	for k := range fresh {
		keys[k] = true
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	var out strings.Builder
	for _, name := range names {
		a, aok := old[name]
		b, bok := fresh[name]
		if aok && bok && a.Mode == b.Mode && bytes.Equal(a.Data, b.Data) {
			continue
		}
		fmt.Fprintf(&out, "diff --git a/%s b/%s\n", name, name)
		if !aok {
			fmt.Fprintf(&out, "new file mode 100%o\n", b.Mode)
		} else if !bok {
			fmt.Fprintf(&out, "deleted file mode 100%o\n", a.Mode)
		} else if a.Mode != b.Mode {
			fmt.Fprintf(&out, "old mode 100%o\nnew mode 100%o\n", a.Mode, b.Mode)
		}
		if bytes.ContainsRune(a.Data, 0) || bytes.ContainsRune(b.Data, 0) {
			aa := sha256.Sum256(a.Data)
			bb := sha256.Sum256(b.Data)
			fmt.Fprintf(&out, "Binary files differ (sha256 %s -> %s)\n", hex.EncodeToString(aa[:]), hex.EncodeToString(bb[:]))
			continue
		}
		an, bn := "a/"+name, "b/"+name
		if !aok {
			an = "/dev/null"
		}
		if !bok {
			bn = "/dev/null"
		}
		fmt.Fprintf(&out, "--- %s\n+++ %s\n", an, bn)
		aLines := textLines(a.Data)
		bLines := textLines(b.Data)
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", lineStart(len(aLines)), len(aLines), lineStart(len(bLines)), len(bLines))
		for _, line := range aLines {
			writeDiffLine(&out, "-", line)
		}
		for _, line := range bLines {
			writeDiffLine(&out, "+", line)
		}
	}
	return out.String()
}

func textLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	lines := strings.SplitAfter(string(data), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
func lineStart(n int) int {
	if n == 0 {
		return 0
	}
	return 1
}
func writeDiffLine(out *strings.Builder, prefix, line string) {
	out.WriteString(prefix)
	out.WriteString(line)
	if !strings.HasSuffix(line, "\n") {
		out.WriteString("\n\\ No newline at end of file\n")
	}
}
