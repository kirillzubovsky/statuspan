package agentdoc

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSkillMatchesSite(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(file), "..", "..", "site", "skill.md")
	site, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skip("site/skill.md absent")
		}
		t.Fatal(err)
	}
	if !bytes.Equal(site, []byte(Skill())) {
		t.Fatal("site/skill.md is not byte-identical to internal/agentdoc/SKILL.md")
	}
}

func TestPromptHasNoFrontmatter(t *testing.T) {
	got := Prompt()
	if strings.HasPrefix(strings.TrimSpace(got), "---") {
		t.Fatal("prompt starts with frontmatter")
	}
	if strings.Contains(got, "name: statuspan") {
		t.Fatal("prompt contains skill frontmatter")
	}
	if !strings.Contains(got, "statuspan run") || !strings.Contains(got, "statuspan post") || !strings.Contains(got, "statuspan agent") {
		t.Fatalf("prompt missing examples:\n%s", got)
	}
}

func TestDiscoveryJSON(t *testing.T) {
	raw, err := DiscoveryJSON("0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(raw, []byte("\n")) {
		t.Fatal("missing trailing newline")
	}
	again, err := DiscoveryJSON("0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, again) {
		t.Fatal("discovery JSON is not stable")
	}
	var doc struct {
		Version  string `json:"version"`
		Commands []struct {
			Name string `json:"name"`
		} `json:"commands"`
		States struct {
			Sendable []string `json:"sendable"`
			Derived  []string `json:"derived"`
		} `json:"states"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != "0.2.0" {
		t.Fatalf("version %q", doc.Version)
	}
	names := map[string]bool{}
	for _, c := range doc.Commands {
		names[c.Name] = true
	}
	if !names["run"] || !names["agent"] {
		t.Fatalf("commands %+v", doc.Commands)
	}
	if !containsAll(doc.States.Sendable, "queued", "running", "blocked", "done", "failed") {
		t.Fatalf("sendable %v", doc.States.Sendable)
	}
	if !containsAll(doc.States.Derived, "stalled", "abandoned") {
		t.Fatalf("derived %v", doc.States.Derived)
	}
}

func containsAll(have []string, want ...string) bool {
	set := map[string]bool{}
	for _, s := range have {
		set[s] = true
	}
	for _, s := range want {
		if !set[s] {
			return false
		}
	}
	return true
}
