package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"agentowl/backend/internal/integration"
)

func TestBridgeReadOnlyAgentWorkspaceTools(t *testing.T) {
	c, fake, _ := fixture(t, nil)
	spec := specFor("edit")
	spec.ExecutionProfile = &ExecutionProfile{NetworkMode: "bridge", ReadOnlyRootFS: true}
	if err := storeSnapshot(c.workspace(spec), spec.Files); err != nil {
		t.Fatal(err)
	}
	a := &activeRun{spec: spec}
	read := integration.ToolCall{Name: "read_file", Arguments: json.RawMessage(`{"path":"main.go"}`)}
	got, err := c.tool(context.Background(), a, read)
	if err != nil || got != string(spec.Files[0].Data) {
		t.Fatalf("read: %q %v", got, err)
	}
	write := integration.ToolCall{Name: "write_file", Arguments: json.RawMessage(`{"path":"main.go","content":"fixed\n","executable":true}`)}
	if _, err := c.tool(context.Background(), a, write); err != nil {
		t.Fatal(err)
	}
	file, err := os.ReadFile(filepath.Join(c.workspace(spec), "main.go"))
	if err != nil || string(file) != "fixed\n" {
		t.Fatalf("bind source was not updated: %q %v", file, err)
	}
	info, err := os.Stat(filepath.Join(c.workspace(spec), "main.go"))
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatal("executable mode not preserved", err)
	}
	if fake.archiveCalls != 0 {
		t.Fatal("known unsupported profile used Docker archive API")
	}
}

func TestAgentCannotWriteIntroducedRedactionMarkers(t *testing.T) {
	c, _, _ := fixture(t, nil)
	spec := specFor("edit")
	spec.ExecutionProfile = &ExecutionProfile{NetworkMode: "bridge", ReadOnlyRootFS: true}
	if err := storeSnapshot(c.workspace(spec), spec.Files); err != nil {
		t.Fatal(err)
	}
	call := integration.ToolCall{Name: "write_file", Arguments: json.RawMessage(`{"path":"main.go","content":"password: [REDACTED]"}`)}
	if _, err := c.tool(context.Background(), &activeRun{spec: spec}, call); err == nil {
		t.Fatal("redaction marker was written into source")
	}
	got, err := os.ReadFile(filepath.Join(c.workspace(spec), "main.go"))
	if err != nil || string(got) != string(spec.Files[0].Data) {
		t.Fatal("original file changed", err)
	}
}

func TestAgentWorkspacePreservesScopeQuotaAndPathBoundaries(t *testing.T) {
	for _, scenario := range []string{"plan", "outside_scope", "byte_quota", "file_quota", "parent_link", "file_link", "directory", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			c, _, _ := fixture(t, nil)
			spec := specFor("edit")
			spec.ExecutionProfile = &ExecutionProfile{NetworkMode: "bridge", ReadOnlyRootFS: true}
			if err := storeSnapshot(c.workspace(spec), spec.Files); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			path, content := "main.go", "changed source"
			outside := t.TempDir()
			protected := filepath.Join(outside, "protected")
			if err := os.WriteFile(protected, []byte("untouched"), 0644); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "plan":
				spec.Stage = "plan"
			case "outside_scope":
				path = "other.go"
			case "byte_quota":
				spec.Limits.WorkspaceBytes = 1
			case "file_quota":
				spec.Limits.MaxFiles = 1
				spec.AllowedPaths = []string{"."}
				path = "new.go"
			case "parent_link":
				spec.AllowedPaths = []string{"."}
				path = "link/protected"
				if err := os.Symlink(outside, filepath.Join(c.workspace(spec), "link")); err != nil {
					t.Fatal(err)
				}
			case "file_link":
				_ = os.Remove(filepath.Join(c.workspace(spec), path))
				if err := os.Symlink(protected, filepath.Join(c.workspace(spec), path)); err != nil {
					t.Fatal(err)
				}
			case "directory":
				_ = os.Remove(filepath.Join(c.workspace(spec), path))
				if err := os.Mkdir(filepath.Join(c.workspace(spec), path), 0755); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			arguments, _ := json.Marshal(map[string]any{"path": path, "content": content, "executable": false})
			if _, err := c.tool(ctx, &activeRun{spec: spec}, integration.ToolCall{Name: "write_file", Arguments: arguments}); err == nil {
				t.Fatal("unsafe or unauthorized write accepted")
			}
			if data, err := os.ReadFile(protected); err != nil || string(data) != "untouched" {
				t.Fatal("external file was changed")
			}
			if scenario == "parent_link" || scenario == "file_link" || scenario == "directory" {
				arguments, _ = json.Marshal(map[string]any{"path": path})
				if _, err := c.tool(ctx, &activeRun{spec: spec}, integration.ToolCall{Name: "read_file", Arguments: arguments}); err == nil {
					t.Fatal("linked or non-regular file read accepted")
				}
			}
		})
	}
}
