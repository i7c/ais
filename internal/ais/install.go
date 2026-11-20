package ais

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	sectionBegin      = "<!-- ais:begin -->"
	sectionEnd        = "<!-- ais:end -->"
	piExtensionMarker = "ais:generated"
)

type harnessSpec struct {
	config       string
	teach        func(dry, force bool)
	instructions func() string
	refreshedAt  string
}

func claudeDir() string { return envOr("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude")) }
func piDir() string     { return envOr("PI_CODING_AGENT_DIR", filepath.Join(home, ".pi", "agent")) }

// Everything "ais install" needs to know about a harness: the config block
// that teaches restart how to resume it, how it is made to report the
// conversation it is in, where its global instructions live, and when a
// harness already running picks the wiring up.
var harnesses = map[string]harnessSpec{
	"claude": {claudeSessionConfig, installHook, func() string { return filepath.Join(claudeDir(), "CLAUDE.md") }, "/clear, /compact or restart"},
	"pi":     {piSessionConfig, installExtension, func() string { return filepath.Join(piDir(), "AGENTS.md") }, "/new, /resume, /fork or restart"},
}

func knownHarnesses() string { return strings.Join(sortedKeys(harnesses), " or ") }

// instructions are the house rules, rendered for one harness.
func instructions(harness string) string {
	return strings.ReplaceAll(strings.ReplaceAll(instructionsText, "{notify_lead}", notifyLead[harness]), "{harness}", harness)
}

// aisCommand is how a hook should invoke ais: by name when PATH has it,
// else by path.
func aisCommand() string {
	if _, err := exec.LookPath(prog); err == nil {
		return prog
	}
	return self()
}

func backup(path string, dry bool) {
	if dry || !exists(path) {
		return
	}
	if st, err := os.Stat(path); err == nil {
		copyFile(path, filepath.Join(filepath.Dir(path), filepath.Base(path)+".ais.bak"), st.Mode())
	}
}

func report(action, path, detail string) {
	line := fmt.Sprintf("%12s  %s", action, tilde(path))
	if detail != "" {
		line += "  " + detail
	}
	fmt.Println(line)
}

// installConfig teaches ais's own config how a harness names conversations.
func installConfig(harness, block string, dry bool) {
	loadConfig() // writes the starter config when there is none
	data, err := os.ReadFile(configPath)
	must(err)
	text := string(data)
	tbl := "[profile." + harness + ".session]"
	// The starter config ships the table commented out as documentation, so
	// only a live one counts as already installed — under either spelling.
	for _, spelling := range []string{tbl, "[harness." + harness + ".session]"} {
		if regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(spelling)).MatchString(text) {
			report("unchanged", configPath, spelling+" already set")
			return
		}
	}
	if dry {
		report("would add", configPath, tbl)
		return
	}
	backup(configPath, dry)
	must(os.WriteFile(configPath, []byte(strings.TrimRight(text, "\n")+"\n"+block), 0o644))
	report("added", configPath, tbl)
}

// claudeHooks are the claude hook events ais listens on, and what it runs.
// SessionStart fires on startup, resume, /clear and /compact — every moment
// the conversation id can change; UserPromptSubmit on every prompt you send.
var claudeHooks = [][2]string{
	{"SessionStart", "_session-seen"},
	{"UserPromptSubmit", "_activity"},
}

// installHook wires claude's hooks to ais without disturbing the rest of
// settings.json, key order included.
func installHook(dry, force bool) {
	path := filepath.Join(claudeDir(), "settings.json")
	settings := &object{}
	if data, err := os.ReadFile(path); err == nil {
		v, err := decodeOrdered(data)
		if err != nil {
			die("%s is not valid JSON (%v); fix it before installing", tilde(path), err)
		}
		obj, ok := v.(*object)
		if !ok {
			die("%s does not contain a JSON object", tilde(path))
		}
		settings = obj
	}
	hooksV := settings.setDefault("hooks", &object{})
	hooks, ok := hooksV.(*object)
	if !ok {
		die(`%s: "hooks" is not an object; fix it before installing`, tilde(path))
	}
	changed := false
	for _, ev := range claudeHooks {
		if wireHook(path, hooks, ev[0], ev[1], dry, force) {
			changed = true
		}
	}
	if !changed || dry {
		return
	}
	backup(path, dry)
	must(os.MkdirAll(filepath.Dir(path), 0o755))
	tmp := path + ".tmp"
	must(os.WriteFile(tmp, append(encodeOrdered(settings), '\n'), 0o644))
	must(os.Rename(tmp, path))
}

// wireHook makes event run "ais <sub>", reporting what it did; true when it
// changed the settings.
func wireHook(path string, hooks *object, event, sub string, dry, force bool) bool {
	command := aisCommand() + " " + sub
	listV := hooks.setDefault(event, &array{})
	list, ok := listV.(*array)
	if !ok {
		die(`%s: "hooks.%s" is not an array`, tilde(path), event)
	}
	var mine []*object
	for _, m := range list.items {
		matcher, ok := m.(*object)
		if !ok {
			continue
		}
		inner, _ := matcher.get("hooks").(*array)
		if inner == nil {
			continue
		}
		for _, h := range inner.items {
			if hook, ok := h.(*object); ok && strings.Contains(fmt.Sprint(hook.get("command")), sub) {
				mine = append(mine, hook)
			}
		}
	}
	if len(mine) > 0 && !force {
		for _, h := range mine {
			if c, _ := h.get("command").(string); c != command {
				report("check", path, fmt.Sprintf("%s runs %s (--force rewrites it)", event, pyRepr(fmt.Sprint(h.get("command")))))
				return false
			}
		}
		report("unchanged", path, event+" hook already installed")
		return false
	}
	if len(mine) > 0 {
		for _, h := range mine {
			h.set("command", command)
		}
	} else {
		hook := &object{}
		hook.set("type", "command")
		hook.set("command", command)
		matcher := &object{}
		matcher.set("hooks", &array{items: []any{hook}})
		list.items = append(list.items, matcher)
	}
	if dry {
		report("would wire", path, event+" -> "+command)
	} else {
		report("wired", path, event+" -> "+command)
	}
	return true
}

func piExtension() string {
	quoted, _ := json.Marshal(aisCommand())
	return strings.ReplaceAll(strings.ReplaceAll(piExtensionText, "{marker}", piExtensionMarker), "{ais}", string(quoted))
}

// installExtension drops the extension that tells ais which pi session is
// running. The whole file is ais's, so it is rewritten whenever its content
// moves on; a file of that name ais did not write belongs to someone else,
// and --force is the only way past it.
func installExtension(dry, force bool) {
	path := filepath.Join(piDir(), "extensions", "ais.ts")
	wanted := piExtension()
	data, err := os.ReadFile(path)
	existing, had := string(data), err == nil
	detail := "session_start -> ais _session-seen, input -> ais _activity"
	if had && existing == wanted {
		report("unchanged", path, "session_start extension already current")
		return
	}
	if had && !strings.Contains(existing, piExtensionMarker) && !force {
		report("check", path, "ais did not write this file (--force overwrites it)")
		return
	}
	if dry {
		report(map[bool]string{true: "would rewrite", false: "would write"}[had], path, detail)
		return
	}
	backup(path, dry)
	must(os.MkdirAll(filepath.Dir(path), 0o755))
	must(os.WriteFile(path, []byte(wanted), 0o644))
	report(map[bool]string{true: "rewrote", false: "wrote"}[had], path, detail)
}

// installInstructions puts the house rules in the harness's global
// instructions, inside ais's markers, so the rest of the file is the user's.
func installInstructions(harness, path string, dry bool) {
	block := sectionBegin + "\n" + instructions(harness) + sectionEnd + "\n"
	data, _ := os.ReadFile(path)
	text := string(data)
	var updated, action string
	if strings.Contains(text, sectionBegin) && strings.Contains(text, sectionEnd) {
		head, rest, _ := strings.Cut(text, sectionBegin)
		current, tail, _ := strings.Cut(rest, sectionEnd)
		updated = head + block + strings.TrimLeft(tail, "\n")
		action = "replaced"
		// Only the section is ais's: when it already says the right thing,
		// the file is left alone, blank lines around it included (a setup
		// that generates the file may well put them there).
		if sectionBegin+current+sectionEnd+"\n" == block {
			action = "unchanged"
		}
	} else {
		prefix := ""
		if strings.TrimSpace(text) != "" {
			prefix = strings.TrimRight(text, "\n") + "\n\n"
		}
		updated, action = prefix+block, "appended"
	}
	if action == "unchanged" {
		report("unchanged", path, "ais section already current")
		return
	}
	if dry {
		report(map[string]string{"appended": "would append", "replaced": "would replace"}[action], path, "ais section")
		return
	}
	backup(path, dry)
	must(os.MkdirAll(filepath.Dir(path), 0o755))
	must(os.WriteFile(path, []byte(updated), 0o644))
	report(action, path, fmt.Sprintf("ais section (%s … %s)", sectionBegin, sectionEnd))
}

func cmdInstall(harness string, dry, force bool) int {
	spec, ok := harnesses[harness]
	if !ok {
		die("%s install knows how to wire up %s, not %s", prog, knownHarnesses(), pyRepr(harness))
	}
	if _, err := exec.LookPath(harness); err != nil {
		note("%s is not on PATH; installing the configuration anyway", harness)
	}
	installConfig(harness, spec.config, dry)
	spec.teach(dry, force)
	installInstructions(harness, spec.instructions(), dry)
	if !dry {
		fmt.Println()
		note("new %s sessions will record their conversation id; existing ones pick it up at the next %s", harness, spec.refreshedAt)
	}
	return 0
}

// cmdInstructions prints the house rules for a harness, for a setup that
// assembles its instructions files itself and must not keep a second copy.
func cmdInstructions(harness string, markers bool) int {
	if _, ok := harnesses[harness]; !ok {
		die("%s has instructions for %s, not %s", prog, knownHarnesses(), pyRepr(harness))
	}
	text := instructions(harness)
	if markers {
		text = sectionBegin + "\n" + text + sectionEnd + "\n"
	}
	fmt.Print(text)
	return 0
}

// ---------------------------------------------------------------- ordered JSON

// object and array keep a JSON document's key order, so rewriting someone's
// settings.json changes what ais added and nothing else.
type object struct {
	keys []string
	vals map[string]any
}

type array struct{ items []any }

func (o *object) get(k string) any {
	if o.vals == nil {
		return nil
	}
	return o.vals[k]
}

func (o *object) set(k string, v any) {
	if o.vals == nil {
		o.vals = map[string]any{}
	}
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *object) setDefault(k string, v any) any {
	if cur, ok := o.vals[k]; ok {
		return cur
	}
	o.set(k, v)
	return v
}

func decodeOrdered(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing data")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := &object{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				o.set(kt.(string), v)
			}
			_, err := dec.Token()
			return o, err
		case '[':
			a := &array{items: []any{}}
			for dec.More() {
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				a.items = append(a.items, v)
			}
			_, err := dec.Token()
			return a, err
		}
	}
	return tok, nil
}

func encodeOrdered(v any) []byte {
	var b bytes.Buffer
	writeOrdered(&b, v, "")
	return b.Bytes()
}

func writeOrdered(b *bytes.Buffer, v any, indent string) {
	inner := indent + "  "
	switch x := v.(type) {
	case *object:
		if len(x.keys) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		for i, k := range x.keys {
			b.WriteString(inner)
			writeScalar(b, k)
			b.WriteString(": ")
			writeOrdered(b, x.vals[k], inner)
			if i < len(x.keys)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		b.WriteString(indent + "}")
	case *array:
		if len(x.items) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i, item := range x.items {
			b.WriteString(inner)
			writeOrdered(b, item, inner)
			if i < len(x.items)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		b.WriteString(indent + "]")
	default:
		writeScalar(b, x)
	}
}

func writeScalar(b *bytes.Buffer, v any) {
	if n, ok := v.(json.Number); ok {
		b.WriteString(n.String())
		return
	}
	var tmp bytes.Buffer
	enc := json.NewEncoder(&tmp)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	b.Write(bytes.TrimRight(tmp.Bytes(), "\n"))
}
