package platform

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestSecretFilesAndConfiguration(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("AUTH_MODE", "local")
	t.Setenv("ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	file := filepath.Join(t.TempDir(), "secret")
	if e := os.WriteFile(file, []byte("from-file\n"), 0600); e != nil {
		t.Fatal(e)
	}
	t.Setenv("BOOTSTRAP_PASSWORD_FILE", file)
	t.Setenv("BOOTSTRAP_PASSWORD", "env-must-not-win")
	c, e := LoadConfig()
	if e != nil {
		t.Fatal(e)
	}
	if c.BootstrapPassword != "from-file" {
		t.Fatal("file priority failed")
	}
	t.Setenv("BOOTSTRAP_PASSWORD_FILE", file+"-missing")
	if _, e = LoadConfig(); e == nil {
		t.Fatal("unreadable secret file was ignored")
	}
}

func TestModelEnvironmentIsNoLongerConfiguration(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("AUTH_MODE", "local")
	t.Setenv("ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("MODEL_API_KEY_FILE", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("MODEL_BASE_URL", "invalid-model-address")
	t.Setenv("MODEL_NAME", "ignored")
	t.Setenv("MODEL_JSON_MODE", "invalid")
	t.Setenv("MAX_DAILY_TOKENS", "invalid")
	if _, err := LoadConfig(); err != nil {
		t.Fatalf("retired model environment still affects startup: %v", err)
	}
}

func TestPublicOrigin(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("AUTH_MODE", "local")
	t.Setenv("ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	for _, invalid := range []string{"https://user:password@example.test", "https://example.test/path", "https://example.test?redirect=x", "https://example.test#fragment", "file:///tmp/app"} {
		t.Run(invalid, func(t *testing.T) {
			t.Setenv("PUBLIC_URL", invalid)
			if _, err := LoadConfig(); err == nil {
				t.Fatal("accepted a URL that is not an HTTP origin")
			}
		})
	}
	t.Setenv("PUBLIC_URL", "https://example.test/")
	c, err := LoadConfig()
	if err != nil || c.PublicURL != "https://example.test" {
		t.Fatalf("valid configuration failed: %v", err)
	}
}

func TestBootstrapRuntimeSecrets(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("AUTH_MODE", "local")
	t.Setenv("ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("BOOTSTRAP_RUNTIME_MACHINE_ID", "a00b7368-a9a0-46af-b7af-16660cf113a6")
	file := filepath.Join(t.TempDir(), "runner-token")
	token := "01234567890123456789012345678901"
	if err := os.WriteFile(file, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RUNNER_SERVICE_TOKEN_FILE", file)
	t.Setenv("RUNNER_CALLBACK_TOKEN_FILE", file)
	c, err := LoadConfig()
	if err != nil || c.BootstrapRuntimeToken != token || c.BootstrapRuntimeCallbackToken != token {
		t.Fatalf("runtime secrets not loaded: %v", err)
	}
	if err := os.WriteFile(file, []byte("short"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(); err == nil {
		t.Fatal("short runtime token accepted")
	}
	t.Setenv("BOOTSTRAP_RUNTIME_MACHINE_ID", "")
	t.Setenv("RUNNER_SERVICE_TOKEN_FILE", file+"-missing")
	if _, err := LoadConfig(); err != nil {
		t.Fatalf("existing deployment requires runtime secrets: %v", err)
	}
}
