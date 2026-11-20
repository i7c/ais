package acceptance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func (l *Lab) sinkPayloads() []map[string]any {
	l.t.Helper()
	var out []map[string]any
	for i := 1; ; i++ {
		path := filepath.Join(l.Log, "sink-"+itoa(i)+".json")
		if !fileExists(path) {
			return out
		}
		var m map[string]any
		must(l.t, json.Unmarshal([]byte(l.ReadFile(path)), &m))
		out = append(out, m)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

// The sink is offered every notification first, with the documented payload,
// and its answer is what gets recorded.
func TestNotifyThroughSink(t *testing.T) {
	l := NewLab(t)
	s := l.Start(l.Home, "run", "fake", "-d", "Sink check")
	in := []string{"AIS_SESSION=" + s.ID}

	r := l.MustAis(in, "notify", "the", "deps", "sweep", "finished")
	p := l.sinkPayloads()
	if len(p) != 1 {
		t.Fatalf("sink offered %d payloads", len(p))
	}
	requireKeys(t, "sink payload", p[0], "id", "at", "title", "message", "tag", "session", "harness", "pane", "visible", "force", "host")
	if p[0]["message"] != "the deps sweep finished" || p[0]["session"] != s.ID || p[0]["pane"] != s.Pane ||
		p[0]["harness"] != "fake" || p[0]["visible"] != false || p[0]["force"] != false {
		t.Errorf("payload = %v", p[0])
	}
	if !strings.Contains(p[0]["title"].(string), "fake") {
		t.Errorf("title %q does not name the harness", p[0]["title"])
	}
	if fileExists(filepath.Join(l.Log, "notify-send")) {
		t.Errorf("ais raised its own banner although the sink showed it")
	}
	n := l.Session(s.ID).Notes
	if len(n) != 1 || !n[0].Delivered || n[0].Suppressed || n[0].ID != p[0]["id"] {
		t.Errorf("recorded %+v (%s)", n, r)
	}

	l.WriteFile(filepath.Join(l.Log, "sink.result"), "suppressed")
	l.MustAis(in, "notify", "quiet one")
	if n := l.Session(s.ID).Notes; !n[1].Suppressed || n[1].Delivered {
		t.Errorf("sink said suppressed, recorded %+v", n[1])
	}

	// A declining sink hands the notification back to ais.
	l.WriteFile(filepath.Join(l.Log, "sink.result"), "decline")
	l.MustAis(in, "notify", "-t", "Custom title", "fallback")
	banner := l.ReadFile(filepath.Join(l.Log, "notify-send"))
	if !strings.Contains(banner, "Custom title") || !strings.Contains(banner, "fallback") {
		t.Errorf("notify-send got:\n%s", banner)
	}
	if n := l.Session(s.ID).Notes; !n[2].Delivered {
		t.Errorf("fallback not recorded as delivered: %+v", n[2])
	}
	l.Stop(s, 0)
}

// No sink, nothing to show it with: recorded anyway, and the exit says so.
func TestNotifyWithoutBackend(t *testing.T) {
	l := NewLab(t)
	l.SetConfig(strings.Replace(l.BaseConfig(), "sink = [", "# sink = [", 1))
	must(t, os.Remove(filepath.Join(l.Bin, "notify-send")))
	s := l.Start(l.Home, "run", "fake")
	r := l.Ais([]string{"AIS_SESSION=" + s.ID}, "notify", "nobody hears this")
	if r.Code == 0 {
		t.Errorf("notify with no way to show it exited 0")
	}
	if n := l.Session(s.ID).Notes; len(n) != 1 || n[0].Delivered || n[0].Message != "nobody hears this" {
		t.Errorf("recorded %+v", n)
	}
	if r := l.Ais(nil, "notify"); r.Code == 0 {
		t.Errorf("notify with no message succeeded")
	}
	l.Stop(s, 0)
}

// Outside a session a notification is shown but has nowhere to be recorded.
func TestNotifyOutsideSession(t *testing.T) {
	l := NewLab(t)
	l.MustAis(nil, "notify", "from a plain shell")
	if p := l.sinkPayloads(); len(p) != 1 || p[0]["session"] != "" {
		t.Errorf("payloads = %v", p)
	}
}

// With the session's pane on screen there is no banner, unless forced; when
// the pane comes on screen its notifications count as seen.
func TestNotifyVisibleAndSeen(t *testing.T) {
	l := NewLab(t)
	l.SetConfig(strings.Replace(l.BaseConfig(), "sink = [", "# sink = [", 1))
	l.Attach()
	s := l.Start(l.Home, "run", "fake", "-d", "On screen")
	in := []string{"AIS_SESSION=" + s.ID}
	banners := filepath.Join(l.Log, "notify-send")

	// Not on screen (the client shows window 0): a banner.
	l.MustAis(in, "notify", "while away")
	if !fileExists(banners) {
		t.Fatalf("no banner for a pane that is not on screen")
	}
	if n := l.Session(s.ID).Notes[0]; n.Seen {
		t.Errorf("seen before it was ever on screen")
	}

	// Bring it on screen: the tmux hooks ais installed mark it seen.
	l.Show(s.Pane)
	l.Eventually("notification marked seen", func() bool { return l.Session(s.ID).Notes[0].Seen })

	must(t, os.Remove(banners))
	l.MustAis(in, "notify", "while looking")
	if fileExists(banners) {
		t.Errorf("banner raised for the pane on screen")
	}
	if n := l.Session(s.ID).Notes[1]; !n.Suppressed || n.Delivered {
		t.Errorf("recorded %+v", n)
	}
	l.MustAis(in, "notify", "-f", "forced")
	if !fileExists(banners) {
		t.Errorf("-f did not force a banner")
	}
	l.Stop(s, 0)
}

// _mark-seen --pane marks a pane's notifications seen without waiting.
func TestMarkSeenPane(t *testing.T) {
	l := NewLab(t)
	s := l.Start(l.Home, "run", "fake")
	l.MustAis([]string{"AIS_SESSION=" + s.ID}, "notify", "look at me")
	l.MustAis(nil, "_mark-seen", "--pane", s.Pane)
	if n := l.Session(s.ID).Notes[0]; !n.Seen || n.SeenAt == "" {
		t.Errorf("not seen: %+v", n)
	}
	l.Stop(s, 0)
}

// ls marks a session that called recently; status shows what it said.
func TestLsAndStatus(t *testing.T) {
	l := NewLab(t)
	if r := l.MustAis(nil, "ls"); strings.Contains(r.Stdout, "ID") {
		t.Errorf("ls with no sessions printed a table:\n%s", r.Stdout)
	}
	quiet := l.Start(l.Home, "run", "fake", "-d", "quiet one")
	loud := l.Start(l.Home, "run", "fake", "-d", "loud one")
	l.MustAis([]string{"AIS_SESSION=" + loud.ID}, "notify", "need you")

	ls := l.MustAis(nil, "ls").Stdout
	for _, id := range []string{quiet.ID, loud.ID} {
		if !strings.Contains(ls, id) {
			t.Errorf("ls lacks %s:\n%s", id, ls)
		}
	}
	for _, line := range lines(ls) {
		if strings.Contains(line, loud.ID) && !strings.HasPrefix(line, "●") {
			t.Errorf("calling session not marked: %q", line)
		}
	}
	if st := l.MustAis(nil, "status").Stdout; !strings.Contains(st, "need you") {
		t.Errorf("status lacks the message:\n%s", st)
	}
	l.Stop(quiet, 0)
	if strings.Contains(l.MustAis(nil, "ls").Stdout, quiet.ID) {
		t.Errorf("ls lists a dead session")
	}
	if !strings.Contains(l.MustAis(nil, "ls", "-a").Stdout, quiet.ID) {
		t.Errorf("ls -a lacks a dead session")
	}
	l.Stop(loud, 0)
}
