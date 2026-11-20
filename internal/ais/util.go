// Package ais is the ais command: it starts a process from a named profile,
// records the run in sessions.json, and finds its tmux pane again later.
//
// This is a port of the Python ais it replaces, held to the same contract by
// the black-box suite in acceptance/. Where the Python had a reason for doing
// something a particular way, the comment saying so came along.
package ais

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const prog = "ais"

// exit is what die panics with; Main recovers it into an exit code, so
// deferred cleanups (window names, pane marks) still run on the way out, as
// Python's context managers did on SystemExit.
type exit struct {
	code int
	msg  string
}

func die(format string, args ...any) {
	panic(exit{code: 1, msg: fmt.Sprintf(format, args...)})
}

func dieCode(code int, format string, args ...any) {
	panic(exit{code: code, msg: fmt.Sprintf(format, args...)})
}

func note(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s: %s\n", prog, fmt.Sprintf(format, args...))
}

// Paths, from $AIS_HOME (default ~/.ais), read once per process.
var (
	home, _     = os.UserHomeDir()
	aisHome     = envOr("AIS_HOME", filepath.Join(home, ".ais"))
	configPath  = filepath.Join(aisHome, "config.toml")
	sessionPath = filepath.Join(aisHome, "sessions.json")
	lockPath    = filepath.Join(aisHome, "sessions.lock")
	wtBase      = filepath.Join(home, "wt")
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func tilde(path string) string {
	if home != "" && strings.HasPrefix(path, home) {
		return "~" + path[len(home):]
	}
	return path
}

// The timestamp format sessions.json has always used: local time, seconds,
// numeric offset ("+00:00", never "Z").
const stampLayout = "2006-01-02T15:04:05-07:00"

func nowISO() string { return time.Now().Format(stampLayout) }

func parseStamp(text string) (time.Time, bool) {
	if text == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, stampLayout} {
		if t, err := time.Parse(layout, text); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func secondsSince(text string) (float64, bool) {
	t, ok := parseStamp(text)
	if !ok {
		return 0, false
	}
	return time.Since(t).Seconds(), true
}

func ageStr(secs float64, ok bool) string {
	if !ok {
		return "-"
	}
	s := int(secs)
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm", s/60)
	case s < 86400:
		return fmt.Sprintf("%dh", s/3600)
	}
	return fmt.Sprintf("%dd", s/86400)
}

func elide(text string, width int) string {
	r := []rune(text)
	if len(r) <= width {
		return text
	}
	return string(r[:width-1]) + "…"
}

// expand is os.path.expanduser(os.path.expandvars(s)): $VAR and ${VAR} are
// replaced when set and left alone when not, then a leading ~ becomes $HOME.
func expand(s string) string {
	s = os.Expand(s, func(name string) string {
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		return "${" + name + "}"
	})
	return expandUser(s)
}

func expandUser(s string) string {
	if s == "~" {
		return home
	}
	if strings.HasPrefix(s, "~/") {
		return filepath.Join(home, s[2:])
	}
	return s
}

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}
