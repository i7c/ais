package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ShellJoin quotes argv for a POSIX shell, leaving plain words bare so the
// line reads the way you would have typed it.
func ShellJoin(argv []string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = shellQuote(a)
	}
	return strings.Join(out, " ")
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			strings.ContainsRune("-_./=:@%+,", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var home, _ = os.UserHomeDir()

func tilde(path string) string {
	if home != "" && (path == home || strings.HasPrefix(path, home+"/")) {
		return "~" + path[len(home):]
	}
	return path
}

func ageStr(d time.Duration) string {
	switch s := int(d.Seconds()); {
	case s < 0:
		return "-"
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm", s/60)
	case s < 86400:
		return fmt.Sprintf("%dh", s/3600)
	default:
		return fmt.Sprintf("%dd", s/86400)
	}
}

func since(stamp string, now time.Time) (time.Duration, bool) {
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return 0, false
	}
	return now.Sub(t), true
}
