package runner

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestExecutionProfileDefaultsAndDigest(t *testing.T) {
	spec := specFor("test")
	wire, err := json.Marshal(spec)
	if err != nil || strings.Contains(string(wire), "execution_profile") {
		t.Fatalf("nil profile changed wire shape: %s %v", wire, err)
	}
	defaultDigest, _ := SpecDigest(spec)
	profile := DefaultExecutionProfile()
	if executionProfile(spec) != profile {
		t.Fatal("nil profile changed isolation defaults")
	}
	spec.ExecutionProfile = &profile
	explicit, _ := SpecDigest(spec)
	if explicit == defaultDigest {
		t.Fatal("an explicit approval profile is not bound to the digest")
	}
	c, _, _ := fixture(t, nil)
	for _, network := range []string{"", "host", "container:other", "bridge "} {
		profile.NetworkMode = network
		if err := c.validate(spec); err == nil {
			t.Fatalf("accepted network mode %q", network)
		}
	}
	profile = DefaultExecutionProfile()
	spec.Limits.TimeoutSeconds = -1
	if err := c.validate(spec); err == nil {
		t.Fatal("negative timeout accepted")
	}
	spec.Limits.TimeoutSeconds = 0
	if err := c.validate(spec); err != nil {
		t.Fatal("disabled timeout rejected", err)
	}
}

func TestExecutionProfileDockerConfiguration(t *testing.T) {
	for _, stage := range []string{"test", "edit", "plan", "review"} {
		for _, enabled := range []bool{false, true} {
			t.Run(stage+"/relaxed="+map[bool]string{false: "false", true: "true"}[enabled], func(t *testing.T) {
				c, fake, _ := fixture(t, nil)
				spec := specFor(stage)
				if enabled {
					spec.ExecutionProfile = &ExecutionProfile{NetworkMode: "bridge", ReadOnlyRootFS: false, TestWorkspaceWritable: true}
				}
				if _, err := c.engine.Create(context.Background(), spec, "profile-test", "/approved/source", "digest", ResourceLimitsRequired); err != nil {
					t.Fatal(err)
				}
				fake.mu.Lock()
				defer fake.mu.Unlock()
				body := fake.createBody
				host := body["HostConfig"].(map[string]any)
				profile := executionProfile(spec)
				if host["NetworkMode"] != profile.NetworkMode || host["ReadonlyRootfs"] != profile.ReadOnlyRootFS || body["NetworkDisabled"] != !enabled {
					t.Fatalf("wrong Docker profile: %v", host)
				}
				if body["User"] != "65532:65532" || !reflect.DeepEqual(host["CapDrop"], []any{"ALL"}) || !reflect.DeepEqual(host["SecurityOpt"], []any{"no-new-privileges:true"}) {
					t.Fatalf("profile relaxed user/capability isolation: %v", body)
				}
				for _, key := range []string{"PortBindings", "PublishAllPorts", "Privileged", "PidMode", "IpcMode", "Devices", "Binds"} {
					if _, present := host[key]; present {
						t.Fatalf("profile unexpectedly set %s", key)
					}
				}
				mounts := host["Mounts"].([]any)
				if len(mounts) != 1 {
					t.Fatalf("unexpected mounts: %v", mounts)
				}
				mount := mounts[0].(map[string]any)
				if enabled && stage == "test" {
					if mount["Type"] != "volume" || mount["Source"] != nil || mount["ReadOnly"] != false || mount["VolumeOptions"].(map[string]any)["NoCopy"] != true {
						t.Fatalf("test did not get its own empty volume: %v", mount)
					}
					if body["WorkingDir"] != "/workspace/source" {
						t.Fatal("test working directory is not the owned volume child")
					}
				} else if mount["Type"] != "bind" || mount["Source"] != "/approved/source" || mount["ReadOnly"] != (stage != "edit") {
					t.Fatalf("unexpected source permissions: %v", mount)
				}
			})
		}
	}
}

func TestExecutionProfileSettingsBindingAndCapabilities(t *testing.T) {
	settings := configuredSettings()
	settings.ExecutionProfile = ExecutionProfile{NetworkMode: "bridge", TestWorkspaceWritable: true}
	provider := &mutableSettings{value: settings}
	c, _, _ := fixtureConfig(t, nil, Config{SettingsProvider: provider})
	ctx := context.Background()
	caps := c.Capabilities(ctx)
	if !caps.Ready || caps.NetworkMode != "bridge" || caps.ReadOnlyRootFS || !caps.TestWorkspaceWritable {
		t.Fatalf("capabilities omitted configured profile: %+v", caps)
	}
	spec := configuredSpec("test", settings)
	for _, change := range []func(*RunSpec){
		func(s *RunSpec) { s.ExecutionProfile = nil },
		func(s *RunSpec) { s.ExecutionProfile.NetworkMode = "none" },
		func(s *RunSpec) { s.ExecutionProfile.ReadOnlyRootFS = true },
		func(s *RunSpec) { s.ExecutionProfile.TestWorkspaceWritable = false },
	} {
		candidate := spec
		profile := *spec.ExecutionProfile
		candidate.ExecutionProfile = &profile
		change(&candidate)
		if _, _, err := c.settingsForRun(ctx, candidate); !errors.Is(err, ErrConflict) {
			t.Fatalf("unapproved profile mismatch accepted: %v", err)
		}
	}
	if _, _, err := c.settingsForRun(ctx, spec); err != nil {
		t.Fatal(err)
	}
	settings.ExecutionProfile.NetworkMode = ""
	provider.set(settings, nil)
	if _, err := c.currentSettings(ctx); err == nil {
		t.Fatal("missing callback profile silently relaxed defaults")
	}
}

func TestWritableTestWorkspacePreservesApprovedSnapshot(t *testing.T) {
	for _, mutation := range []string{"generated_files", "changed_source", "deleted_source", "executable_changed", "linked_source"} {
		t.Run(mutation, func(t *testing.T) {
			c, fake, _ := fixture(t, nil)
			spec := specFor("test")
			spec.ExecutionProfile = &ExecutionProfile{NetworkMode: "none", ReadOnlyRootFS: true, TestWorkspaceWritable: true}
			fake.onStart = func(root string) {
				if root == c.workspace(spec) {
					t.Error("test received original snapshot")
				}
				if err := os.MkdirAll(filepath.Join(root, "node_modules", "dependency"), 0755); err != nil {
					t.Error(err)
				}
				_ = os.WriteFile(filepath.Join(root, "node_modules", "dependency", "index.js"), []byte("generated dependency"), 0644)
				target := filepath.Join(root, "main.go")
				switch mutation {
				case "changed_source":
					_ = os.WriteFile(target, []byte("changed"), 0644)
				case "deleted_source":
					_ = os.Remove(target)
				case "executable_changed":
					_ = os.Chmod(target, 0755)
				case "linked_source":
					_ = os.WriteFile(filepath.Join(root, "linked.go"), spec.Files[0].Data, 0644)
					_ = os.Remove(target)
					_ = os.Symlink("linked.go", target)
				}
			}
			if _, err := c.Start(context.Background(), spec); err != nil {
				t.Fatal(err)
			}
			run := waitTerminal(t, c, spec.RunID)
			wantSuccess := mutation == "generated_files"
			if (run.State == "succeeded") != wantSuccess || run.Result.EvidenceValid != wantSuccess || run.Result.OutputDigest != spec.InputDigest || run.Result.ExitCode == nil || *run.Result.ExitCode != 0 {
				t.Fatalf("test evidence did not follow original input: %+v", run)
			}
			archive, err := c.Collect(spec.RunID)
			if err != nil {
				t.Fatal(err)
			}
			defer archive.Close()
			gz, err := gzip.NewReader(archive)
			if err != nil {
				t.Fatal(err)
			}
			defer gz.Close()
			entries, err := readArchive(gz, spec.Limits.ArtifactBytes, spec.Limits.MaxFiles+10)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, entry := range entries {
				if strings.Contains(entry.Path, "node_modules") || strings.Contains(entry.Path, "linked.go") {
					t.Fatal("generated test files leaked into artifacts")
				}
				if entry.Path == "workspace/main.go" {
					found = string(entry.Data) == string(spec.Files[0].Data)
				}
				if entry.Path == "diff.patch" && len(entry.Data) != 0 {
					t.Fatal("test modifications leaked into PR diff")
				}
			}
			if !found {
				t.Fatal("original input absent from artifact")
			}
			deadline := time.Now().Add(time.Second)
			for {
				fake.mu.Lock()
				removed, owned := fake.container == nil, fake.seedOwned
				fake.mu.Unlock()
				if !owned {
					t.Fatal("test workspace was not seeded for UID/GID 65532")
				}
				if removed {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("test container and volume were not removed")
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}

func TestDisabledTimeoutStillAllowsStopAndRevocation(t *testing.T) {
	for _, action := range []string{"stop", "revoke"} {
		t.Run(action, func(t *testing.T) {
			c, fake, auth := fixture(t, nil)
			fake.runningTest = true
			spec := specFor("test")
			spec.Limits.TimeoutSeconds = 0
			if _, err := c.Start(context.Background(), spec); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(time.Second)
			for {
				run, err := c.Inspect(spec.RunID)
				if err != nil || terminal(run.State) {
					t.Fatalf("zero timeout cancelled immediately: %+v %v", run, err)
				}
				if run.State == "running" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("test did not start")
				}
				time.Sleep(time.Millisecond)
			}
			if action == "stop" {
				if _, err := c.Stop(context.Background(), spec.RunID); err != nil {
					t.Fatal(err)
				}
			} else {
				auth.deny.Store(true)
			}
			if run := waitTerminal(t, c, spec.RunID); run.State != "stopped" || run.Result.EvidenceValid {
				t.Fatalf("unlimited run did not stop: %+v", run)
			}
		})
	}
}
