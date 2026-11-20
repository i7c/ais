package acceptance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	sectionBegin = "<!-- ais:begin -->"
	sectionEnd   = "<!-- ais:end -->"
)

// install --harness claude: a session table in config, a SessionStart hook
// beside whatever else settings.json holds, and the house rules between
// markers in CLAUDE.md, leaving the rest of the file alone. Idempotent.
func TestInstallClaude(t *testing.T) {
	l := NewLab(t)
	claudeDir := filepath.Join(l.Home, ".claude")
	settings := filepath.Join(claudeDir, "settings.json")
	claudeMD := filepath.Join(claudeDir, "CLAUDE.md")
	config := filepath.Join(l.AisHome, "config.toml")
	l.WriteFile(settings, `{"model": "opus", "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "mine"}]}]}}`)
	l.WriteFile(claudeMD, "# My rules\n\nBe kind.\n")
	before := snapshot(t, settings, claudeMD, config)

	if r := l.MustAis(nil, "install", "--harness", "claude", "-n"); r.Stdout == "" {
		t.Errorf("dry run reported nothing")
	}
	if after := snapshot(t, settings, claudeMD, config); after != before {
		t.Errorf("install -n changed files")
	}

	l.MustAis(nil, "install", "--harness", "claude")
	var s map[string]any
	must(t, json.Unmarshal([]byte(l.ReadFile(settings)), &s))
	if s["model"] != "opus" {
		t.Errorf("install lost an unrelated setting")
	}
	hooks := s["hooks"].(map[string]any)
	if _, ok := hooks["Stop"]; !ok {
		t.Errorf("install lost an existing hook")
	}
	if !strings.Contains(mustJSON(t, hooks["SessionStart"]), `ais _session-seen`) {
		t.Errorf("SessionStart = %s", mustJSON(t, hooks["SessionStart"]))
	}
	md := l.ReadFile(claudeMD)
	if !strings.HasPrefix(md, "# My rules\n\nBe kind.\n") || !strings.Contains(md, sectionBegin) || !strings.Contains(md, sectionEnd) {
		t.Errorf("CLAUDE.md:\n%s", md)
	}
	if !strings.Contains(l.ReadFile(config), "[profile.claude.session]") {
		t.Errorf("config lacks [profile.claude.session]")
	}
	// The section is what "ais instructions --markers" prints.
	section := md[strings.Index(md, sectionBegin):]
	if printed := l.MustAis(nil, "instructions", "--markers").Stdout; !strings.HasPrefix(section, printed) {
		t.Errorf("CLAUDE.md section differs from ais instructions --markers")
	}

	once := snapshot(t, settings, claudeMD, config)
	l.MustAis(nil, "install", "--harness", "claude")
	if twice := snapshot(t, settings, claudeMD, config); twice != once {
		t.Errorf("second install changed files")
	}
	if n := strings.Count(l.ReadFile(claudeMD), sectionBegin); n != 1 {
		t.Errorf("%d ais sections after two installs", n)
	}
}

func TestInstallPi(t *testing.T) {
	l := NewLab(t)
	piDir := filepath.Join(l.Home, ".pi", "agent")
	l.MustAis(nil, "install", "--harness", "pi")
	ext := l.ReadFile(filepath.Join(piDir, "extensions", "ais.ts"))
	if !strings.Contains(ext, "_session-seen") || !strings.Contains(ext, "session_start") {
		t.Errorf("extension:\n%s", ext)
	}
	if md := l.ReadFile(filepath.Join(piDir, "AGENTS.md")); !strings.Contains(md, sectionBegin) {
		t.Errorf("AGENTS.md lacks the ais section")
	}
	if !strings.Contains(l.ReadFile(filepath.Join(l.AisHome, "config.toml")), "[profile.pi.session]") {
		t.Errorf("config lacks [profile.pi.session]")
	}

	// An ais.ts ais did not write is somebody else's, unless --force.
	l.WriteFile(filepath.Join(piDir, "extensions", "ais.ts"), "// not ours\n")
	l.MustAis(nil, "install", "--harness", "pi")
	if got := l.ReadFile(filepath.Join(piDir, "extensions", "ais.ts")); got != "// not ours\n" {
		t.Errorf("install overwrote a foreign extension")
	}
	l.MustAis(nil, "install", "--harness", "pi", "--force")
	if got := l.ReadFile(filepath.Join(piDir, "extensions", "ais.ts")); !strings.Contains(got, "_session-seen") {
		t.Errorf("--force did not take the extension back")
	}
}

func TestInstallAndInstructionsRefusals(t *testing.T) {
	l := NewLab(t)
	if r := l.Ais(nil, "install", "--harness", "emacs"); r.Code == 0 {
		t.Errorf("install of an unknown harness succeeded")
	}
	if r := l.Ais(nil, "instructions", "emacs"); r.Code == 0 {
		t.Errorf("instructions for an unknown harness succeeded")
	}
	claude := l.MustAis(nil, "instructions").Stdout
	pi := l.MustAis(nil, "instructions", "pi").Stdout
	if claude == "" || pi == "" || claude == pi || !strings.Contains(claude, "ais notify") {
		t.Errorf("instructions: claude %d bytes, pi %d bytes", len(claude), len(pi))
	}
	marked := l.MustAis(nil, "instructions", "--markers").Stdout
	if !strings.HasPrefix(marked, sectionBegin+"\n") || !strings.HasSuffix(marked, sectionEnd+"\n") {
		t.Errorf("--markers output not wrapped")
	}
	l.WriteFile(filepath.Join(l.Home, ".claude", "settings.json"), "{not json")
	if r := l.Ais(nil, "install"); r.Code == 0 {
		t.Errorf("install over a broken settings.json succeeded")
	}
}

func snapshot(t *testing.T, paths ...string) string {
	t.Helper()
	var b strings.Builder
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		b.WriteString(p + "\x00" + string(data) + "\x00")
	}
	return b.String()
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	must(t, err)
	return string(b)
}
