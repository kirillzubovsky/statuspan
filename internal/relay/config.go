// Package relay polls a StatusPan relay into the local store and resolves
// daemon configuration. The read token is never logged.
package relay

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	configFile = "relay.json"
	cursorFile = "relay-cursor"
)

// Status is the relay section of /healthz. It never includes a token.
type Status struct {
	Configured bool       `json:"configured"`
	LastOK     *time.Time `json:"last_ok"`
	Cursor     int64      `json:"cursor"`
	LastError  string     `json:"last_error"`
}

// Input is the raw relay settings. Empty strings are unset.
// Flag values win over Env values, which win over relay.json in DataDir.
type Input struct {
	DataDir       string
	FlagURL       string
	FlagWorkspace string
	FlagTokenFile string
	EnvURL        string
	EnvWorkspace  string
	EnvToken      string
}

// Config is a complete relay target. Token is a read token for the daemon.
type Config struct {
	URL       string
	Workspace string
	Token     string
}

var workspaceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type fileConfig struct {
	URL       string `json:"url"`
	Workspace string `json:"workspace"`
	ReadToken string `json:"read_token"`
}

// Resolve picks the relay config. ok is false when nothing is configured.
// A partial configuration is an error so the daemon does not silently go local.
func Resolve(in Input) (Config, bool, error) {
	urlVal := strings.TrimSpace(in.FlagURL)
	if urlVal == "" {
		urlVal = strings.TrimSpace(in.EnvURL)
	}
	workspace := strings.TrimSpace(in.FlagWorkspace)
	if workspace == "" {
		workspace = strings.TrimSpace(in.EnvWorkspace)
	}
	var token string
	if strings.TrimSpace(in.FlagTokenFile) != "" {
		tok, err := ReadTokenFile(in.FlagTokenFile)
		if err != nil {
			return Config{}, false, fmt.Errorf("relay token file: %w", err)
		}
		token = tok
	} else {
		token = strings.TrimSpace(in.EnvToken)
	}
	needFile := urlVal == "" || workspace == "" || token == ""
	if needFile {
		file, present, err := readConfigFile(in.DataDir)
		if err != nil {
			return Config{}, false, err
		}
		if present {
			if urlVal == "" {
				urlVal = strings.TrimSpace(file.URL)
			}
			if workspace == "" {
				workspace = strings.TrimSpace(file.Workspace)
			}
			if token == "" {
				token = strings.TrimSpace(file.ReadToken)
			}
		}
	}
	if urlVal == "" && workspace == "" && token == "" {
		return Config{}, false, nil
	}
	var missing []string
	if urlVal == "" {
		missing = append(missing, "url")
	}
	if workspace == "" {
		missing = append(missing, "workspace")
	}
	if token == "" {
		missing = append(missing, "read token")
	}
	if len(missing) > 0 {
		return Config{}, false, fmt.Errorf("relay config is missing %s", strings.Join(missing, ", "))
	}
	base, err := normalizeBase(urlVal)
	if err != nil {
		return Config{}, false, err
	}
	if !workspaceName.MatchString(workspace) {
		return Config{}, false, fmt.Errorf("relay workspace must match %s", workspaceName.String())
	}
	if err := checkToken(token); err != nil {
		return Config{}, false, err
	}
	return Config{URL: base, Workspace: workspace, Token: token}, true, nil
}

// ReadTokenFile reads a single-line token. The token is not included in errors.
func ReadTokenFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if len(b) > 4096 {
		return "", fmt.Errorf("token file is too large")
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", fmt.Errorf("token file is empty")
	}
	if err := checkToken(token); err != nil {
		return "", err
	}
	return token, nil
}

func checkToken(token string) error {
	if token == "" {
		return fmt.Errorf("token is empty")
	}
	if strings.ContainsAny(token, " \t\r\n") {
		return fmt.Errorf("token must be a single value with no whitespace")
	}
	return nil
}

func normalizeBase(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("relay url must be http or https")
	}
	if u.User != nil {
		return "", fmt.Errorf("relay url must not include user info")
	}
	u.RawQuery = ""
	u.Fragment = ""
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String(), nil
}

func readConfigFile(dir string) (fileConfig, bool, error) {
	if strings.TrimSpace(dir) == "" {
		return fileConfig{}, false, nil
	}
	b, err := os.ReadFile(filepath.Join(dir, configFile))
	if err != nil {
		if os.IsNotExist(err) {
			return fileConfig{}, false, nil
		}
		return fileConfig{}, false, fmt.Errorf("relay.json: %w", err)
	}
	if len(bytesTrim(b)) == 0 {
		return fileConfig{}, false, nil
	}
	if len(b) > 1<<20 {
		return fileConfig{}, false, fmt.Errorf("relay.json is too large")
	}
	var file fileConfig
	if err := json.Unmarshal(b, &file); err != nil {
		return fileConfig{}, false, fmt.Errorf("relay.json: invalid json")
	}
	present := strings.TrimSpace(file.URL) != "" || strings.TrimSpace(file.Workspace) != "" || strings.TrimSpace(file.ReadToken) != ""
	return file, present, nil
}

func bytesTrim(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

// ErrorText returns the relay's error field, or a one-line body snippet.
func ErrorText(body []byte) string {
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &payload) == nil {
		if msg := oneLine(payload.Error); msg != "" {
			return msg
		}
	}
	msg := oneLine(string(body))
	if msg == "" {
		return "request failed"
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return msg
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
