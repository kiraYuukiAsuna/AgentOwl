package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type Engine struct {
	client  *http.Client
	baseURL string
}

func NewEngine(socket string) *Engine {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "unix", socket)
	}, MaxIdleConns: 8, IdleConnTimeout: 30 * time.Second}
	return &Engine{client: &http.Client{Transport: transport, Timeout: 45 * time.Second}, baseURL: "http://docker"}
}

type DockerInfo struct {
	SecurityOptions []string `json:"SecurityOptions"`
	CgroupVersion   string   `json:"CgroupVersion"`
	CgroupDriver    string   `json:"CgroupDriver"`
	DockerRootDir   string   `json:"DockerRootDir"`
	NCPU            int      `json:"NCPU"`
	MemTotal        int64    `json:"MemTotal"`
	MemoryLimit     bool     `json:"MemoryLimit"`
	PidsLimit       bool     `json:"PidsLimit"`
	CPUQuota        bool     `json:"CpuCfsQuota"`
}

type Container struct {
	ID     string           `json:"Id"`
	Name   string           `json:"Name"`
	Mounts []ContainerMount `json:"Mounts"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Status    string `json:"Status"`
		Running   bool   `json:"Running"`
		ExitCode  int    `json:"ExitCode"`
		OOMKilled bool   `json:"OOMKilled"`
		Error     string `json:"Error"`
	} `json:"State"`
}

type ContainerMount struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	Driver      string `json:"Driver"`
	RW          bool   `json:"RW"`
}

type dockerError struct {
	status  int
	message string
}

func (e *dockerError) Error() string { return fmt.Sprintf("docker status %d: %s", e.status, e.message) }
func dockerNotFound(err error) bool {
	var de *dockerError
	return errors.As(err, &de) && de.status == 404
}

func (e *Engine) request(ctx context.Context, method, endpoint string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, e.baseURL+"/v1.45"+endpoint, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 300 {
		defer res.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		var msg struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &msg)
		return nil, &dockerError{status: res.StatusCode, message: msg.Message}
	}
	return res, nil
}

func (e *Engine) json(ctx context.Context, method, endpoint string, body, result any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	res, err := e.request(ctx, method, endpoint, reader, "application/json")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if result != nil {
		return json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(result)
	}
	_, err = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	return err
}

func (e *Engine) Info(ctx context.Context) (DockerInfo, error) {
	var info DockerInfo
	err := e.json(ctx, "GET", "/info", nil, &info)
	return info, err
}
func (e *Engine) Inspect(ctx context.Context, name string) (Container, error) {
	var c Container
	err := e.json(ctx, "GET", "/containers/"+url.PathEscape(name)+"/json", nil, &c)
	return c, err
}
func (e *Engine) ImageExists(ctx context.Context, image string) error {
	return e.json(ctx, "GET", "/images/"+url.PathEscape(image)+"/json", nil, nil)
}
func (e *Engine) Start(ctx context.Context, id string) error {
	return e.json(ctx, "POST", "/containers/"+url.PathEscape(id)+"/start", nil, nil)
}
func (e *Engine) Kill(ctx context.Context, id string) error {
	err := e.json(ctx, "POST", "/containers/"+url.PathEscape(id)+"/kill?signal=SIGKILL", nil, nil)
	var de *dockerError
	if dockerNotFound(err) || (errors.As(err, &de) && de.status == 409) {
		return nil
	}
	return err
}
func (e *Engine) Remove(ctx context.Context, id string) error {
	err := e.json(ctx, "DELETE", "/containers/"+url.PathEscape(id)+"?force=true&v=true", nil, nil)
	if dockerNotFound(err) {
		return nil
	}
	return err
}

func (e *Engine) Managed(ctx context.Context) ([]Container, error) {
	filter := url.QueryEscape(`{"label":["agentowl.managed=true"]}`)
	var rows []struct {
		ID string `json:"Id"`
	}
	if err := e.json(ctx, "GET", "/containers/json?all=true&filters="+filter, nil, &rows); err != nil {
		return nil, err
	}
	var result []Container
	for _, row := range rows {
		c, err := e.Inspect(ctx, row.ID)
		if err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, nil
}

func (e *Engine) Create(ctx context.Context, spec RunSpec, name, hostWorkspace, specDigest, resourceLimits string) (string, error) {
	mode, err := normalizeResourceLimits(resourceLimits)
	if err != nil {
		return "", err
	}
	profile := executionProfile(spec)
	if err := validateExecutionProfile(profile); err != nil {
		return "", err
	}
	env := []string{"HOME=/tmp", "TMPDIR=/tmp", "GOCACHE=/output/go-cache", "GOMODCACHE=/output/go-mod", "NPM_CONFIG_CACHE=/output/npm", "CI=true"}
	keys := make([]string, 0, len(spec.Env))
	for key := range spec.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		env = append(env, key+"="+spec.Env[key])
	}
	command := []string{"/bin/sleep", "infinity"}
	if spec.Stage == "test" {
		command = spec.Argv
	}
	body := map[string]any{
		"Image": spec.Image, "User": "65532:65532", "Cmd": command, "Entrypoint": []string{}, "WorkingDir": "/workspace", "Env": env,
		"AttachStdout": true, "AttachStderr": true, "Tty": false, "NetworkDisabled": profile.NetworkMode == "none",
		"Labels": map[string]string{"agentowl.managed": "true", "agentowl.run_id": spec.RunID, "agentowl.attempt_id": spec.AttemptID, "agentowl.task_id": spec.TaskID, "agentowl.spec_digest": specDigest, "agentowl.epoch": fmt.Sprint(spec.Epoch)},
		"HostConfig": map[string]any{
			"ReadonlyRootfs": profile.ReadOnlyRootFS, "NetworkMode": profile.NetworkMode, "CapDrop": []string{"ALL"}, "SecurityOpt": []string{"no-new-privileges:true"},
			"Mounts": []map[string]any{{"Type": "bind", "Source": hostWorkspace, "Target": "/workspace", "ReadOnly": spec.Stage != "edit", "BindOptions": map[string]any{"Propagation": "rprivate"}}},
			// Linux tmpfs size=0/nr_inodes=0 explicitly disable those quotas;
			// omitting them would silently apply the kernel's smaller defaults.
			"Tmpfs":     map[string]string{"/output": workspaceTmpfs(spec.Limits), "/tmp": workspaceTmpfs(spec.Limits)},
			"Ulimits":   []map[string]any{{"Name": "nofile", "Soft": 1024, "Hard": 1024}},
			"LogConfig": map[string]any{"Type": "json-file", "Config": map[string]string{"max-size": "2m", "max-file": "2"}}, "AutoRemove": false,
		},
	}
	if spec.Limits.WorkspaceBytes > 0 {
		host := body["HostConfig"].(map[string]any)
		host["Ulimits"] = append(host["Ulimits"].([]map[string]any), map[string]any{"Name": "fsize", "Soft": spec.Limits.WorkspaceBytes, "Hard": spec.Limits.WorkspaceBytes})
	}
	if spec.Stage == "test" && profile.TestWorkspaceWritable {
		// An anonymous volume is owned by this container and removed with v=true.
		// Never expose the approved source bind to writable test commands.
		body["HostConfig"].(map[string]any)["Mounts"] = []map[string]any{{"Type": "volume", "Target": "/workspace", "ReadOnly": false, "VolumeOptions": map[string]any{"NoCopy": true}}}
		body["WorkingDir"] = "/workspace/source"
	}
	if mode == ResourceLimitsRequired {
		host := body["HostConfig"].(map[string]any)
		host["Memory"] = spec.Limits.MemoryBytes
		host["MemorySwap"] = spec.Limits.MemoryBytes
		host["NanoCpus"] = spec.Limits.CPUMillis * 1_000_000
		host["PidsLimit"] = spec.Limits.PIDs
	}
	var response struct {
		ID string `json:"Id"`
	}
	err = e.json(ctx, "POST", "/containers/create?name="+url.QueryEscape(name), body, &response)
	return response.ID, err
}

func workspaceTmpfs(limits Limits) string {
	return fmt.Sprintf("rw,nosuid,nodev,size=%d,nr_inodes=%d,mode=1777", limits.WorkspaceBytes, limits.MaxFiles)
}

// SeedTestWorkspace initializes only the dedicated anonymous volume, before any
// repository command starts. Docker performs UID mapping for its non-root user.
func (e *Engine) SeedTestWorkspace(ctx context.Context, id string, files []File) error {
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	// Docker does not apply tar '.' ownership to the volume mount root. A real
	// child directory can be owned by the non-root test user on rootless engines.
	if err := tw.WriteHeader(&tar.Header{Name: "source", Typeflag: tar.TypeDir, Mode: 0755, Uid: 65532, Gid: 65532}); err != nil {
		return err
	}
	dirs := map[string]bool{"source": true}
	for _, file := range files {
		if err := validPath(file.Path); err != nil {
			return err
		}
		parts := strings.Split(file.Path, "/")
		for i := 1; i < len(parts); i++ {
			dir := "source/" + strings.Join(parts[:i], "/")
			if !dirs[dir] {
				if err := tw.WriteHeader(&tar.Header{Name: dir, Typeflag: tar.TypeDir, Mode: 0755, Uid: 65532, Gid: 65532}); err != nil {
					return err
				}
				dirs[dir] = true
			}
		}
		if err := tw.WriteHeader(&tar.Header{Name: "source/" + file.Path, Typeflag: tar.TypeReg, Mode: int64(file.Mode), Size: int64(len(file.Data)), Uid: 65532, Gid: 65532}); err != nil {
			return err
		}
		if _, err := tw.Write(file.Data); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	res, err := e.request(ctx, "PUT", "/containers/"+url.PathEscape(id)+"/archive?path=%2Fworkspace&noOverwriteDirNonDir=1&copyUIDGID=1", &archive, "application/x-tar")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, err = io.Copy(io.Discard, res.Body)
	return err
}

func (e *Engine) ReadFile(ctx context.Context, id, name string, max int64) ([]byte, error) {
	if err := validPath(name); err != nil {
		return nil, err
	}
	res, err := e.request(ctx, "GET", "/containers/"+url.PathEscape(id)+"/archive?path="+url.QueryEscape("/workspace/"+name), nil, "")
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	files, err := readArchive(res.Body, max, 4)
	if err != nil {
		return nil, err
	}
	if len(files) != 1 {
		return nil, errors.New("read requires exactly one regular file")
	}
	return files[0].Data, nil
}

func (e *Engine) WriteFile(ctx context.Context, id, name string, mode uint32, data []byte) error {
	archive, err := fileTar(name, mode, data)
	if err != nil {
		return err
	}
	res, err := e.request(ctx, "PUT", "/containers/"+url.PathEscape(id)+"/archive?path=%2Fworkspace&noOverwriteDirNonDir=1", bytes.NewReader(archive), "application/x-tar")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, err = io.Copy(io.Discard, res.Body)
	return err
}

func (e *Engine) Logs(ctx context.Context, id string, maxBytes int64) ([]byte, error) {
	res, err := e.request(ctx, "GET", "/containers/"+url.PathEscape(id)+"/logs?stdout=true&stderr=true&timestamps=true", nil, "")
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var result bytes.Buffer
	for result.Len() < int(maxBytes) {
		var header [8]byte
		_, err := io.ReadFull(res.Body, header[:])
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if header[0] != 1 && header[0] != 2 {
			return nil, errors.New("invalid docker log stream")
		}
		n := int64(binary.BigEndian.Uint32(header[4:]))
		if n < 0 || n > 16<<20 {
			return nil, errors.New("docker log frame too large")
		}
		remaining := maxBytes - int64(result.Len())
		keep := min64(n, remaining)
		if _, err := io.CopyN(&result, res.Body, keep); err != nil {
			return nil, err
		}
		if n > keep {
			_, _ = io.CopyN(io.Discard, res.Body, n-keep)
			result.WriteString("\n[log limit reached]\n")
			break
		}
	}
	return []byte(redact(result.String())), nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func containerMatches(c Container, s RunSpec, digest string) bool {
	l := c.Config.Labels
	return l["agentowl.managed"] == "true" && l["agentowl.run_id"] == s.RunID && l["agentowl.task_id"] == s.TaskID && l["agentowl.attempt_id"] == s.AttemptID && l["agentowl.epoch"] == fmt.Sprint(s.Epoch) && l["agentowl.spec_digest"] == digest
}

func validateRuntimeIdentity(info DockerInfo, hostRoot string) error {
	rootless := false
	for _, option := range info.SecurityOptions {
		if strings.Contains(option, "rootless") {
			rootless = true
		}
	}
	if !rootless {
		return errors.New("dedicated Docker daemon must run rootless")
	}
	if strings.TrimSuffix(info.DockerRootDir, "/") != strings.TrimSuffix(hostRoot, "/")+"/runtime" {
		return errors.New("Docker data-root does not match dedicated runner runtime")
	}
	return nil
}

func validateInfo(info DockerInfo, hostRoot, resourceLimits string) error {
	mode, err := normalizeResourceLimits(resourceLimits)
	if err != nil {
		return err
	}
	if err := validateRuntimeIdentity(info, hostRoot); err != nil {
		return err
	}
	if mode == ResourceLimitsDisabled {
		return nil
	}
	if info.CgroupVersion != "2" || info.CgroupDriver != "systemd" {
		return errors.New("rootless resource limits require cgroup v2 with systemd")
	}
	if !info.MemoryLimit || !info.PidsLimit || !info.CPUQuota {
		return errors.New("Docker daemon cannot enforce required memory, PID and CPU limits")
	}
	return nil
}
