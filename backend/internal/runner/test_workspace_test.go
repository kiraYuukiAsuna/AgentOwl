package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTestVolumeRequiresOwnedStoppedLocalMount(t *testing.T) {
	c, _, _ := fixture(t, nil)
	spec := specFor("test")
	spec.ExecutionProfile = &ExecutionProfile{NetworkMode: "bridge", ReadOnlyRootFS: true, TestWorkspaceWritable: true}
	digest, _ := SpecDigest(spec)
	id, err := c.engine.Create(context.Background(), spec, containerName(spec.RunID), c.workspace(spec), digest, ResourceLimitsRequired)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.engine.SeedTestWorkspace(context.Background(), id, spec.Files); err != nil {
		t.Fatal(err)
	}
	container, err := c.engine.Inspect(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	a := &activeRun{spec: spec, run: Run{ContainerID: id, SpecDigest: digest}}
	volume, err := c.openTestVolume(a, container)
	if err != nil {
		t.Fatal(err)
	}
	file, err := readTestInputFile(volume, "main.go", int64(len(spec.Files[0].Data)))
	volume.Close()
	if err != nil || string(file.Data) != string(spec.Files[0].Data) {
		t.Fatalf("owned test volume cannot be read: %+v %v", file, err)
	}
	original, _ := json.Marshal(container)
	for name, change := range map[string]func(*Container){
		"different container": func(v *Container) { v.ID = "other" },
		"still running":       func(v *Container) { v.State.Running = true },
		"different task":      func(v *Container) { v.Config.Labels["agentowl.task_id"] = "other" },
		"arbitrary host path": func(v *Container) { v.Mounts[0].Source = "/etc" },
		"path traversal":      func(v *Container) { v.Mounts[0].Name = "../../etc" },
		"non-local driver":    func(v *Container) { v.Mounts[0].Driver = "external" },
		"bind mount":          func(v *Container) { v.Mounts[0].Type = "bind" },
		"missing mount":       func(v *Container) { v.Mounts = nil },
		"ambiguous mount":     func(v *Container) { v.Mounts = append(v.Mounts, v.Mounts[0]) },
	} {
		t.Run(name, func(t *testing.T) {
			var bad Container
			_ = json.Unmarshal(original, &bad)
			change(&bad)
			if volume, err := c.openTestVolume(a, bad); err == nil {
				volume.Close()
				t.Fatal("unowned volume accepted")
			}
		})
	}
	path := filepath.Join(c.cfg.DataRoot, "runtime", "volumes", container.Mounts[0].Name, "_data")
	if err = os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(t.TempDir(), path); err != nil {
		t.Fatal(err)
	}
	if volume, err := c.openTestVolume(a, container); err == nil {
		volume.Close()
		t.Fatal("linked volume root accepted")
	}
}

func TestTestInputReadRejectsLinksAndOversizedFiles(t *testing.T) {
	for _, kind := range []string{"source_link", "parent_link", "file_link", "oversized", "directory", "missing"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			if err := storeSnapshot(filepath.Join(root, "source"), []File{{Path: "src/main.go", Mode: 0644, Data: []byte("safe")}}); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(root, "source", "src", "main.go")
			switch kind {
			case "source_link", "parent_link":
				dir := filepath.Join(root, "source")
				if kind == "parent_link" {
					dir = filepath.Join(dir, "src")
				}
				_ = os.RemoveAll(dir)
				_ = os.Symlink(t.TempDir(), dir)
			case "file_link":
				_ = os.Remove(file)
				_ = os.Symlink("/etc/passwd", file)
			case "oversized":
				_ = os.WriteFile(file, []byte(strings.Repeat("x", 100)), 0644)
			case "directory":
				_ = os.Remove(file)
				_ = os.Mkdir(file, 0755)
			case "missing":
				_ = os.Remove(file)
			}
			volume, err := os.OpenRoot(root)
			if err != nil {
				t.Fatal(err)
			}
			defer volume.Close()
			if _, err := readTestInputFile(volume, "src/main.go", 4); err == nil {
				t.Fatal("invalid approved file was accepted")
			}
		})
	}
}
