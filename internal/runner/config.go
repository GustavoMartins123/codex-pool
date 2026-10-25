package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	PoolURL  string `json:"pool_url"`
	RunnerID string `json:"runner_id"`
	Token    string `json:"token"`
	// CommandKey is shown once in the pairing QR and never sent to the pool.
	CommandKey string `json:"command_key,omitempty"`
}

// LaunchConfig is safe to keep beside the binary: it contains paths and labels,
// never the runner credential. The credential remains in Config at StatePath.
type LaunchConfig struct {
	State      string   `json:"state"`
	PoolURL    string   `json:"pool_url"`
	Name       string   `json:"name"`
	Workspaces string   `json:"workspaces"`
	Codex      string   `json:"codex"`
	Claude     string   `json:"claude"`
	AllowPaths []string `json:"allow_paths"`
	Labels     []string `json:"labels"`
	QR         string   `json:"qr,omitempty"`
	// AllowAuto accepts sessions in auto permission mode, which run the agent
	// without approvals or a filesystem sandbox. Off unless the machine owner
	// opts in here, so a pool server or app credential cannot grant it alone.
	AllowAuto bool `json:"allow_auto,omitempty"`
}

type Options struct {
	Config               Config
	WorkspaceRoot        string
	AllowedPaths         []string
	Labels               []string
	Executables          map[string]string
	HTTPClient           *http.Client
	AllowAutoPermissions bool
}

func ValidateOrigin(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return errors.New("pool URL must be an HTTPS origin without path, credentials, query or fragment")
	}
	return nil
}

func DefaultStatePath() (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "codex-pool", "runnerd", "runner.json"), nil
}

func ReadLaunchConfig(path string) (LaunchConfig, error) {
	var cfg LaunchConfig
	file, err := os.Open(path)
	if err != nil {
		return cfg, errors.New("runner launch config unavailable")
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return cfg, errors.New("invalid runner launch config")
	}
	if !filepath.IsAbs(cfg.State) || cfg.State == "" {
		return cfg, errors.New("runner launch config requires an absolute state path")
	}
	if err := ValidateOrigin(cfg.PoolURL); err != nil {
		return cfg, err
	}
	if cfg.Name == "" || strings.ContainsAny(cfg.Name, "\x00\r\n") || len(cfg.Name) > 256 {
		return cfg, errors.New("runner launch config requires a valid name")
	}
	if !filepath.IsAbs(cfg.Workspaces) || cfg.Workspaces == "" {
		return cfg, errors.New("runner launch config requires an absolute workspace root")
	}
	for _, path := range append(append([]string{cfg.Codex, cfg.Claude}, cfg.AllowPaths...), cfg.State, cfg.Workspaces) {
		if path != "" && strings.ContainsAny(path, "\x00\r\n") {
			return cfg, errors.New("runner launch config contains an invalid path")
		}
	}
	if cfg.Codex == "" && cfg.Claude == "" {
		return cfg, errors.New("runner launch config requires codex or claude")
	}
	for agent, executable := range map[string]string{"codex": cfg.Codex, "claude": cfg.Claude} {
		if executable == "" {
			continue
		}
		if !filepath.IsAbs(executable) {
			return cfg, fmt.Errorf("%s executable path must be absolute", agent)
		}
	}
	for _, path := range cfg.AllowPaths {
		if !filepath.IsAbs(path) {
			return cfg, errors.New("allowed workspace paths must be absolute")
		}
	}
	return cfg, nil
}

func ReadConfig(path string) (Config, error) {
	var cfg Config
	file, err := os.Open(path)
	if err != nil {
		return cfg, errors.New("runner state unavailable; enroll first")
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return cfg, errors.New("invalid runner state")
	}
	if err := ValidateOrigin(cfg.PoolURL); err != nil {
		return cfg, err
	}
	if cfg.RunnerID == "" || !strings.HasPrefix(cfg.Token, "pool-runner-"+cfg.RunnerID+".") {
		return cfg, errors.New("invalid runner credential")
	}
	if cfg.CommandKey != "" {
		if _, err := decodeCommandKey(cfg.CommandKey); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

func WriteConfig(path string, cfg Config) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(parent, ".runner-*.tmp")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if err := privateFile(file); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

func httpClient(client *http.Client) *http.Client {
	if client == nil {
		return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copy
}

func fetchCredential(ctx context.Context, client *http.Client, cfg Config, agent, path string) (string, error) {
	prefix := "/config/" + agent + "/"
	if !strings.HasPrefix(path, prefix) || len(path) != len(prefix)+32 || strings.ContainsAny(strings.TrimPrefix(path, prefix), "/?#%\\\r\n") {
		return "", errors.New("invalid session credential path")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.PoolURL+path, nil)
	if err != nil {
		return "", errors.New("invalid credential request")
	}
	response, err := httpClient(client).Do(request)
	if err != nil {
		return "", errors.New("session credential transport failed")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return "", fmt.Errorf("session credential rejected (HTTP %d)", response.StatusCode)
	}
	var credential struct {
		OpenAIKey   string `json:"OPENAI_API_KEY"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&credential); err != nil {
		return "", errors.New("invalid session credential response")
	}
	var token string
	switch agent {
	case "codex":
		token = credential.OpenAIKey
	case "claude":
		token = credential.AccessToken
	default:
		return "", errors.New("unsupported agent")
	}
	if token == "" || strings.ContainsAny(token, "\x00\r\n") {
		return "", errors.New("empty or invalid session credential")
	}
	return token, nil
}
