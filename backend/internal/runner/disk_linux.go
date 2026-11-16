//go:build linux

package runner

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func diskPreflight(root string, minFree int64) error {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	tasks := filepath.Join(root, "tasks")
	runtime := filepath.Join(root, "runtime")
	taskMount, err := findMount(string(data), tasks)
	if err != nil {
		return err
	}
	runtimeMount, err := findMount(string(data), runtime)
	if err != nil {
		return err
	}
	if taskMount.fs != "tmpfs" || !strings.Contains(taskMount.options, "size=") || !strings.Contains(taskMount.options, "nr_inodes=") {
		return errors.New("runner tasks require bounded tmpfs with explicit size and nr_inodes")
	}
	if runtimeMount.fs != "ext4" && runtimeMount.fs != "xfs" {
		return errors.New("runner runtime requires a dedicated bounded ext4/xfs filesystem")
	}
	if !strings.HasPrefix(runtimeMount.source, "/dev/") || runtimeMount.device == taskMount.device {
		return errors.New("runner runtime must be a dedicated block filesystem")
	}
	rootMount, err := findMount(string(data), root)
	if err != nil {
		return err
	}
	if rootMount.device == runtimeMount.device {
		return errors.New("runtime capacity must be isolated from controller state")
	}
	for _, dir := range []string{tasks, runtime, filepath.Join(root, "manifests")} {
		var st syscall.Statfs_t
		if err := syscall.Statfs(dir, &st); err != nil {
			return err
		}
		if int64(st.Bavail)*int64(st.Bsize) < minFree {
			return fmt.Errorf("runner storage low: %s", filepath.Base(dir))
		}
		if st.Ffree < 1024 {
			return fmt.Errorf("runner inode reserve low: %s", filepath.Base(dir))
		}
	}
	return nil
}

func diskAvailable(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

type mountRecord struct{ target, fs, source, options, device string }

func findMount(data, target string) (mountRecord, error) {
	var selected mountRecord
	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		parts := strings.SplitN(scanner.Text(), " - ", 2)
		if len(parts) != 2 {
			continue
		}
		left, right := strings.Fields(parts[0]), strings.Fields(parts[1])
		if len(left) < 6 || len(right) < 3 {
			continue
		}
		name := unescapeMount(left[4])
		if target != name && !strings.HasPrefix(target, strings.TrimSuffix(name, "/")+"/") {
			continue
		}
		if len(name) < len(selected.target) {
			continue
		}
		selected = mountRecord{target: name, device: left[2], fs: right[0], source: unescapeMount(right[1]), options: left[5] + "," + right[2]}
	}
	if err := scanner.Err(); err != nil {
		return selected, err
	}
	if selected.target == "" {
		return selected, errors.New("mount not found")
	}
	return selected, nil
}
func unescapeMount(s string) string {
	for _, code := range []string{"040", "011", "012", "134"} {
		n, _ := strconv.ParseInt(code, 8, 32)
		s = strings.ReplaceAll(s, "\\"+code, string(rune(n)))
	}
	return s
}
