package platform

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	DatabaseURL, RedisURL, Listen, InternalListen, PublicURL, DataDir, AuthMode     string
	BootstrapEmail, BootstrapPassword, BootstrapName, BootstrapSubject              string
	OIDCIssuer, OIDCClientID, OIDCClientSecret                                      string
	BootstrapRuntimeMachineID, BootstrapRuntimeToken, BootstrapRuntimeCallbackToken string
	EncryptionKey                                                                   []byte
	SessionTTL                                                                      time.Duration
}

func LoadConfig() (Config, error) {
	c := Config{DatabaseURL: os.Getenv("DATABASE_URL"), RedisURL: os.Getenv("REDIS_URL"), Listen: env("LISTEN_ADDR", ":8080"), InternalListen: env("INTERNAL_LISTEN_ADDR", ":8081"), PublicURL: env("PUBLIC_URL", "http://localhost:3000"), DataDir: env("DATA_DIR", "../deploy/data"), AuthMode: env("AUTH_MODE", "oidc"), BootstrapEmail: os.Getenv("BOOTSTRAP_EMAIL"), BootstrapPassword: os.Getenv("BOOTSTRAP_PASSWORD"), BootstrapName: env("BOOTSTRAP_NAME", "Administrator"), BootstrapSubject: os.Getenv("BOOTSTRAP_SUBJECT"), OIDCIssuer: os.Getenv("OIDC_ISSUER"), OIDCClientID: os.Getenv("OIDC_CLIENT_ID"), OIDCClientSecret: os.Getenv("OIDC_CLIENT_SECRET"), SessionTTL: 12 * time.Hour}
	for key, dest := range map[string]*string{"DATABASE_URL": &c.DatabaseURL, "BOOTSTRAP_PASSWORD": &c.BootstrapPassword, "OIDC_CLIENT_SECRET": &c.OIDCClientSecret} {
		v, e := secretEnv(key)
		if e != nil {
			return c, e
		}
		*dest = v
	}
	c.BootstrapRuntimeMachineID = os.Getenv("BOOTSTRAP_RUNTIME_MACHINE_ID")
	if c.BootstrapRuntimeMachineID != "" {
		for key, dest := range map[string]*string{"RUNNER_SERVICE_TOKEN": &c.BootstrapRuntimeToken, "RUNNER_CALLBACK_TOKEN": &c.BootstrapRuntimeCallbackToken} {
			value, err := secretEnv(key)
			if err != nil {
				return c, err
			}
			if len(value) < 32 {
				return c, fmt.Errorf("%s must contain at least 32 characters", key)
			}
			*dest = value
		}
	}
	key, err := secretEnv("ENCRYPTION_KEY")
	if err != nil {
		return c, err
	}
	c.EncryptionKey, err = base64.StdEncoding.DecodeString(key)
	if err != nil || len(c.EncryptionKey) != 32 {
		return c, errors.New("ENCRYPTION_KEY must be a base64-encoded 32-byte key")
	}
	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	u, e := url.Parse(c.PublicURL)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "http" && u.Scheme != "https") {
		return c, errors.New("PUBLIC_URL must be an HTTP(S) origin")
	}
	c.PublicURL = strings.TrimSuffix(c.PublicURL, "/")
	if c.AuthMode != "local" && c.AuthMode != "oidc" {
		return c, errors.New("AUTH_MODE must be local or oidc")
	}
	if c.AuthMode == "oidc" && (c.OIDCIssuer == "" || c.OIDCClientID == "" || c.OIDCClientSecret == "" || c.BootstrapSubject == "") {
		return c, errors.New("OIDC configuration and BOOTSTRAP_SUBJECT are required")
	}
	c.DataDir, e = filepath.Abs(c.DataDir)
	return c, e
}
func env(key, fallback string) string {
	if s := os.Getenv(key); s != "" {
		return s
	}
	return fallback
}

func secretEnv(key string) (string, error) {
	if file := os.Getenv(key + "_FILE"); file != "" {
		b, e := os.ReadFile(file)
		if e != nil {
			return "", fmt.Errorf("cannot read %s_FILE", key)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return os.Getenv(key), nil
}
