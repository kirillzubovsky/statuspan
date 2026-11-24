package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kirillzubovsky/statuspan/internal/agentdoc"
)

func TestAgentPrintAndSkillAlias(t *testing.T) {
	want := agentdoc.Skill()
	if !strings.HasPrefix(want, "---\n") || !strings.Contains(want, "name: statuspan") {
		t.Fatal("embedded skill missing frontmatter")
	}
	for _, args := range [][]string{{"agent"}, {"agent", "print"}, {"agent", "skill"}, {"skill"}} {
		got, err := captureStdout(t, func() error { return run(args) })
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%v output mismatch", args)
		}
	}
}

func TestAgentPromptAndJSON(t *testing.T) {
	prompt, err := captureStdout(t, func() error { return run([]string{"agent", "prompt"}) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(strings.TrimSpace(prompt), "---") || strings.Contains(prompt, "name: statuspan") {
		t.Fatalf("prompt has frontmatter:\n%s", prompt)
	}
	if !strings.Contains(prompt, "statuspan run") || !strings.Contains(prompt, "statuspan post") || !strings.Contains(prompt, "statuspan agent") {
		t.Fatalf("prompt:\n%s", prompt)
	}

	raw, err := captureStdout(t, func() error { return run([]string{"agent", "json"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(raw, "\n") {
		t.Fatal("json missing trailing newline")
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
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != version {
		t.Fatalf("version %q", doc.Version)
	}
	names := map[string]bool{}
	for _, c := range doc.Commands {
		names[c.Name] = true
	}
	if !names["run"] || !names["agent"] {
		t.Fatalf("commands %+v", doc.Commands)
	}
	if len(doc.States.Sendable) == 0 || len(doc.States.Derived) == 0 {
		t.Fatalf("states %+v", doc.States)
	}
}

func TestAgentHelp(t *testing.T) {
	got, err := captureStdout(t, func() error { return run([]string{"agent", "--help"}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"prompt", "json", "install", "--dry-run", "--target", "--dir"} {
		if !strings.Contains(got, needle) {
			t.Fatalf("help missing %q\n%s", needle, got)
		}
	}
}

func TestAgentInstallDir(t *testing.T) {
	dir := t.TempDir()
	got, err := captureStdout(t, func() error {
		return run([]string{"agent", "install", "--dir", dir})
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "SKILL.md")
	if got != path+"\n" {
		t.Fatalf("stdout %q", got)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != agentdoc.Skill() {
		t.Fatal("installed skill mismatch")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
}

func TestAgentInstallAutoHome(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := captureStdout(t, func() error {
		return run([]string{"agent", "install", "--target", "auto", "--home", home})
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(home, ".agents", "skills", "statuspan", "SKILL.md"),
		filepath.Join(home, ".claude", "skills", "statuspan", "SKILL.md"),
		filepath.Join(home, ".codex", "skills", "statuspan", "SKILL.md"),
	}
	if got != strings.Join(want, "\n")+"\n" {
		t.Fatalf("stdout %q", got)
	}
	cursor := filepath.Join(home, ".cursor", "skills", "statuspan", "SKILL.md")
	if _, err := os.Stat(cursor); !os.IsNotExist(err) {
		t.Fatal("cursor skill was written")
	}
	for _, path := range want {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o644 {
			t.Fatalf("%s mode %o", path, info.Mode().Perm())
		}
		dirInfo, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if dirInfo.Mode().Perm() != 0o755 {
			t.Fatalf("%s dir mode %o", path, dirInfo.Mode().Perm())
		}
	}
}

func TestAgentInstallHomeEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.Mkdir(filepath.Join(home, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := captureStdout(t, func() error { return run([]string{"agent", "install"}) })
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(home, ".agents", "skills", "statuspan", "SKILL.md"),
		filepath.Join(home, ".cursor", "skills", "statuspan", "SKILL.md"),
	}
	if got != strings.Join(want, "\n")+"\n" {
		t.Fatalf("stdout %q", got)
	}
	for _, path := range want {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
		t.Fatal("claude dir was created")
	}
}

func TestAgentInstallExplicitTargets(t *testing.T) {
	home := t.TempDir()
	got, err := captureStdout(t, func() error {
		return run([]string{"agent", "install", "--home", home, "--target", "codex,cursor"})
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(home, ".codex", "skills", "statuspan", "SKILL.md"),
		filepath.Join(home, ".cursor", "skills", "statuspan", "SKILL.md"),
	}
	if got != strings.Join(want, "\n")+"\n" {
		t.Fatalf("stdout %q", got)
	}
	for _, path := range want {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".agents")); !os.IsNotExist(err) {
		t.Fatal("agents dir was created")
	}
}

func TestAgentInstallDryRun(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(t.TempDir(), "skills")
	got, err := captureStdout(t, func() error {
		return run([]string{"agent", "install", "--home", home, "--dry-run"})
	})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".agents", "skills", "statuspan", "SKILL.md")
	if got != "would install "+want+"\n" {
		t.Fatalf("stdout %q", got)
	}
	if _, err := os.Stat(filepath.Join(home, ".agents")); !os.IsNotExist(err) {
		t.Fatal("dry-run created a directory")
	}
	got, err = captureStdout(t, func() error {
		return run([]string{"agent", "install", "--dir", dir, "--dry-run"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "would install "+filepath.Join(dir, "SKILL.md")+"\n" {
		t.Fatalf("dir dry-run %q", got)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("dry-run created --dir")
	}
}

func TestAgentInstallUnknownTarget(t *testing.T) {
	err := run([]string{"agent", "install", "--home", t.TempDir(), "--target", "nope"})
	if err == nil || !strings.Contains(err.Error(), "unknown agent target") {
		t.Fatal(err)
	}
}
