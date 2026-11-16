package runner

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// openTestVolume reads only the local volume allocated to this stopped run.
// Rootless Docker on some cgroup-v1 hosts cannot GET archive from a stopped,
// read-only bridge container: remounting its resolv.conf fails with EPERM. The
// Controller already has this dedicated data root mounted. Derive its local path
// rather than trusting an arbitrary Mount.Source returned by the engine.
func (c *Controller) openTestVolume(a *activeRun, container Container) (*os.Root, error) {
	if container.ID != a.run.ContainerID || container.State.Running || !containerMatches(container, a.spec, a.run.SpecDigest) {
		return nil, errors.New("test container identity or stopped state does not match")
	}
	var selected *ContainerMount
	for i := range container.Mounts {
		mount := &container.Mounts[i]
		if mount.Destination == "/workspace" {
			if selected != nil {
				return nil, errors.New("ambiguous test workspace mount")
			}
			selected = mount
		}
	}
	if selected == nil || selected.Type != "volume" || selected.Driver != "local" || !selected.RW || !digestPattern.MatchString(selected.Name) {
		return nil, errors.New("test workspace is not an owned local anonymous volume")
	}
	relative := filepath.Join("volumes", selected.Name, "_data")
	if selected.Source != filepath.Join(c.cfg.HostDataRoot, "runtime", relative) {
		return nil, errors.New("test workspace mount is outside the dedicated runtime")
	}
	// The deployment root can be traverse-only (0711). The dedicated daemon's
	// runtime directory is owned by the Controller/daemon account and readable.
	runtime := filepath.Join(c.cfg.DataRoot, "runtime")
	info, err := os.Lstat(runtime)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("dedicated runtime path is missing, linked or not a directory")
	}
	root, err := os.OpenRoot(runtime)
	if err != nil {
		return nil, errors.New("dedicated runtime is unavailable")
	}
	defer root.Close()
	part := ""
	for _, element := range strings.Split(relative, string(filepath.Separator)) {
		part = filepath.Join(part, element)
		info, err := root.Lstat(part)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("test volume path is missing, linked or not a directory")
		}
	}
	volume, err := root.OpenRoot(relative)
	if err != nil {
		return nil, errors.New("test volume is unavailable")
	}
	return volume, nil
}

func readTestInputFile(volume *os.Root, name string, max int64) (File, error) {
	if err := validPath(name); err != nil {
		return File{}, err
	}
	file, err := readBoundedWorkspaceFile(volume, "source/"+name, max)
	file.Path = name
	return file, err
}

func readBoundedWorkspaceFile(volume *os.Root, name string, max int64) (File, error) {
	if err := validPath(name); err != nil {
		return File{}, err
	}
	if max < 0 {
		return File{}, errors.New("invalid test input byte bound")
	}
	relative := name
	part := ""
	var info os.FileInfo
	for index, element := range strings.Split(relative, "/") {
		part = filepath.Join(part, element)
		current, err := volume.Lstat(part)
		if err != nil {
			return File{}, errors.New("approved test input is missing or inaccessible")
		}
		if current.Mode()&os.ModeSymlink != 0 {
			return File{}, errors.New("approved test input contains a symbolic link")
		}
		if index < strings.Count(relative, "/") && !current.IsDir() {
			return File{}, errors.New("approved test input parent is not a directory")
		}
		info = current
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > max {
		return File{}, errors.New("approved test input is non-regular or oversized")
	}
	file, err := volume.Open(relative)
	if err != nil {
		return File{}, errors.New("approved test input cannot be read")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return File{}, errors.New("approved test input changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, addSize(max, 1)))
	if err != nil || int64(len(data)) > max {
		return File{}, errors.New("approved test input exceeds its byte bound or cannot be read")
	}
	mode := uint32(0644)
	if opened.Mode()&0111 != 0 {
		mode = 0755
	}
	return File{Path: name, Mode: mode, Data: data}, nil
}
