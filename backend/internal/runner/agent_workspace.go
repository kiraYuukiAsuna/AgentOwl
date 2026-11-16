package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
)

// Bridge plus a read-only rootfs breaks Docker archive mounts on some rootless
// hosts. These file-only operations use the same per-attempt bind source already
// used by list/delete and snapshot collection. There is no host command runner or
// fallback for other Docker failures, and each operation remains below os.Root.
func (c *Controller) openAgentWorkspace(spec RunSpec) (*os.Root, error) {
	for _, id := range []string{spec.TaskID, spec.AttemptID} {
		if !idPattern.MatchString(id) {
			return nil, errors.New("invalid workspace identity")
		}
	}
	name := c.workspace(spec)
	actual, err := filepath.EvalSymlinks(name)
	if err != nil || actual != filepath.Clean(name) {
		return nil, errors.New("task workspace is unavailable or linked")
	}
	return os.OpenRoot(name)
}

func (c *Controller) readAgentWorkspace(ctx context.Context, spec RunSpec, name string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := c.openAgentWorkspace(spec)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	limit := spec.Limits.WorkspaceBytes
	if limit == 0 {
		limit = math.MaxInt64
	}
	file, err := readBoundedWorkspaceFile(root, name, limit)
	return file.Data, err
}

func (c *Controller) writeAgentWorkspace(ctx context.Context, spec RunSpec, name string, mode uint32, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if spec.Stage != "edit" || !pathAllowed(name, spec.AllowedPaths) || validPath(name) != nil || spec.Limits.WorkspaceBytes > 0 && int64(len(data)) > spec.Limits.WorkspaceBytes || mode != 0644 && mode != 0755 {
		return errors.New("file write is outside approved edit scope")
	}
	root, err := c.openAgentWorkspace(spec)
	if err != nil {
		return err
	}
	defer root.Close()
	parent := ""
	parts := strings.Split(name, "/")
	for _, component := range parts[:len(parts)-1] {
		parent = filepath.Join(parent, component)
		info, err := root.Lstat(parent)
		if os.IsNotExist(err) {
			if err = root.Mkdir(parent, 0755); err != nil {
				return err
			}
			continue
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("file parent is not an unlinked directory")
		}
	}
	if info, err := root.Lstat(name); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("only regular workspace files can be replaced")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	tmp := filepath.Join(parent, ".agentowl-write-"+hex.EncodeToString(nonce[:]))
	file, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	if _, err = file.Write(data); err == nil {
		err = file.Chmod(os.FileMode(mode))
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return root.Rename(tmp, name)
}
