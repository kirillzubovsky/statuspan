package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kirillzubovsky/statuspan/internal/agentdoc"
)

func agentCmd(args []string) error {
	if len(args) == 0 {
		fmt.Print(agentdoc.Skill())
		return nil
	}
	switch args[0] {
	case "print", "skill":
		if len(args) > 1 {
			return fmt.Errorf("agent %s takes no arguments", args[0])
		}
		fmt.Print(agentdoc.Skill())
		return nil
	case "prompt":
		if len(args) > 1 {
			return fmt.Errorf("agent prompt takes no arguments")
		}
		fmt.Print(agentdoc.Prompt())
		return nil
	case "json":
		if len(args) > 1 {
			return fmt.Errorf("agent json takes no arguments")
		}
		body, err := agentdoc.DiscoveryJSON(version)
		if err != nil {
			return err
		}
		fmt.Print(string(body))
		return nil
	case "install":
		return agentInstall(args[1:])
	case "help", "-h", "--help", "-help":
		agentUsage()
		return nil
	default:
		return fmt.Errorf("unknown agent command %q (try statuspan agent --help)", args[0])
	}
}

func agentUsage() {
	fmt.Fprintf(os.Stdout, `statuspan agent — instructions for AI agents

  statuspan agent              print the full skill
  statuspan agent print        same as statuspan agent
  statuspan agent skill        same as statuspan agent
  statuspan agent prompt       short snippet for AGENTS.md or CLAUDE.md
  statuspan agent json         discovery document as JSON
  statuspan agent install      install the skill into agent directories
  statuspan skill              alias of statuspan agent

Install flags:
  --target auto|agents|claude|cursor|codex|all
                         comma-separated list; default auto
  --dir PATH             write PATH/SKILL.md instead of the tool targets
  --home PATH            home directory (default: the user's home)
  --dry-run              print target paths and write nothing

auto always installs ~/.agents/skills/statuspan/SKILL.md, and also installs
~/.claude, ~/.cursor, and ~/.codex skills when those directories already exist.
An explicit target is created even when the directory is new.
all writes agents, claude, cursor, and codex.

`)
}

func agentInstall(args []string) error {
	fs := newFlagSet("agent install")
	fs.Usage = func() { agentUsage() }
	target := fs.String("target", "auto", "auto, agents, claude, cursor, codex, all, or a comma-separated list")
	dir := fs.String("dir", "", "write PATH/SKILL.md instead of the tool targets")
	home := fs.String("home", "", "home directory (default: the user's home)")
	dry := fs.Bool("dry-run", false, "print target paths and write nothing")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	var paths []string
	if strings.TrimSpace(*dir) != "" {
		paths = []string{filepath.Join(*dir, "SKILL.md")}
	} else {
		homeDir := strings.TrimSpace(*home)
		if homeDir == "" {
			var err error
			homeDir, err = os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("home directory: %w", err)
			}
		}
		var err error
		paths, err = skillTargets(homeDir, *target)
		if err != nil {
			return err
		}
	}
	for _, path := range paths {
		if *dry {
			fmt.Printf("would install %s\n", path)
			continue
		}
		if err := writeSkillAtomic(path); err != nil {
			return err
		}
		fmt.Println(path)
	}
	return nil
}

// skillTargets resolves install paths. auto includes claude, cursor, and codex
// only when that directory already exists under home.
func skillTargets(home, spec string) ([]string, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		spec = "auto"
	}
	var names []string
	seen := map[string]bool{}
	add := func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		names = append(names, name)
	}
	for _, part := range strings.Split(spec, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			return nil, fmt.Errorf("unknown agent target %q", part)
		}
		switch name {
		case "auto":
			add("agents")
			for _, tool := range []string{"claude", "cursor", "codex"} {
				fi, err := os.Stat(filepath.Join(home, "."+tool))
				if err == nil && fi.IsDir() {
					add(tool)
				}
			}
		case "all":
			add("agents")
			add("claude")
			add("cursor")
			add("codex")
		case "agents", "claude", "cursor", "codex":
			add(name)
		default:
			return nil, fmt.Errorf("unknown agent target %q", name)
		}
	}
	paths := make([]string, 0, len(names))
	for _, name := range names {
		paths = append(paths, filepath.Join(home, "."+name, "skills", "statuspan", "SKILL.md"))
	}
	return paths, nil
}

func writeSkillAtomic(path string) error {
	dir := filepath.Dir(path)
	if err := mkdirAll0755(dir); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".statuspan-skill-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.WriteString(agentdoc.Skill()); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(0o644); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// mkdirAll0755 creates path and forces mode 0755 on directories that did not
// already exist. Existing directories are left alone.
func mkdirAll0755(path string) error {
	var created []string
	dir := path
	for {
		if _, err := os.Stat(dir); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return err
		}
		created = append(created, dir)
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	for _, d := range created {
		if err := os.Chmod(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}
