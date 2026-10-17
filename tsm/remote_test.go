package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSSHNeverPromptsAndSharesOneConnection(t *testing.T) {
	r := Remote{Conf: RemoteConf{Name: "karif", SSH: "karif.example"}}
	argv := r.argv("true")
	line := strings.Join(argv, " ")
	for _, want := range []string{"ControlMaster=auto", "ControlPersist=8h", "BatchMode=yes", "karif.example -- true"} {
		if !strings.Contains(line, want) {
			t.Errorf("ssh line lacks %q: %s", want, line)
		}
	}
}

func TestTheRemoteTsmIsFoundWherePathDoesNotSay(t *testing.T) {
	line := Remote{Conf: RemoteConf{TSM: "~/bin/tsm"}}.tsmLine("_capture", "%1")
	if !strings.Contains(line, `"$HOME"/bin/tsm _capture %1`) || !strings.HasPrefix(line, `PATH="$HOME/.local/bin:`) {
		t.Fatal(line)
	}
	if line := (Remote{}).tsmLine("_op"); !strings.HasSuffix(line, " tsm _op") {
		t.Fatal(line)
	}
}

func TestAttachCarriesTheLink(t *testing.T) {
	ssh := attachArgv(RemoteConf{SSH: "karif", Attach: "ssh", Session: "main"}, "laptop:%3")
	if ssh[0] != "ssh" || ssh[1] != "-t" || ssh[len(ssh)-1] != "env TSM_LINK=laptop:%3 tmux new-session -A -s main" {
		t.Fatalf("ssh: %v", ssh)
	}
	mosh := attachArgv(RemoteConf{SSH: "karif", Attach: "mosh", Session: "main"}, "laptop:%3")
	if !reflect.DeepEqual(mosh, []string{"mosh", "karif", "--", "env", "TSM_LINK=laptop:%3", "tmux", "new-session", "-A", "-s", "main"}) {
		t.Fatalf("mosh: %v", mosh)
	}
}

func TestTheConnectionIsFoundByItsCommandLine(t *testing.T) {
	conf := RemoteConf{Name: "karif", SSH: "karif"}
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"ssh", "karif"}, true},
		{[]string{"/usr/bin/ssh", "-t", "me@karif"}, true},
		{[]string{"mosh-client", "-#", "karif | 1.2.3.4 60001", "1.2.3.4", "60001"}, true},
		{[]string{"ssh", "karifx"}, false},
		{[]string{"vim", "karif"}, false},
	} {
		if got := namesHost(tc.args, conf); got != tc.want {
			t.Errorf("%v: %v", tc.args, got)
		}
	}
}

func TestAttachMarksWin(t *testing.T) {
	snap := fixture()
	snap.Pane("%4").Remote = "karif"
	if p := hostingPane(snap, RemoteConf{Name: "karif", SSH: "karif"}); p == nil || p.ID != "%4" {
		t.Fatal("the pane tsm attach marked is the way there")
	}
}

func TestAnOpFindsItsRowAgainOnTheOtherEnd(t *testing.T) {
	snap := fixture()
	st := &AisState{API: 1, Host: "h", Sessions: []AisSession{{ID: "aaa", Status: "active", Host: "h"}}}
	p := snap.Pane("%1")
	op := OpFor("duplicate", Row{Kind: RowWindow, Session: p.Window.Session, Window: p.Window, Pane: p})
	data, _ := json.Marshal(op)
	var back Op
	json.Unmarshal(data, &back)
	r, err := back.row(fixture(), st)
	if err != nil || r.Pane.ID != "%1" || r.Ais == nil || r.Ais.ID != "aaa" {
		t.Fatalf("row %+v, err %v", r, err)
	}
	back.Pane = "%9"
	if _, err := back.row(fixture(), st); err == nil {
		t.Fatal("a pane that has gone is said to be gone")
	}
}

func TestRemotesGetSectionsAndKeysOfTheirOwn(t *testing.T) {
	now := time.Now()
	h := &Host{Name: "karif", Snap: fixture(), State: &AisState{API: 1, Host: "k", NotifyWindow: 60, Sessions: []AisSession{{
		ID: "aaa", Status: "active", Host: "k",
		Notifications: []AisNotification{{At: now.Format(time.RFC3339), Message: "hey"}},
	}}}}
	rows := BuildAll(fixture(), nil, []*Host{h}, TreeOptions{Now: now})
	keys := map[string]bool{}
	var header, wants bool
	for _, r := range rows {
		if keys[r.Key()] {
			t.Fatalf("key %s twice: the same pane id on two hosts must not collide", r.Key())
		}
		keys[r.Key()] = true
		header = header || (r.Kind == RowHeader && r.Host == h)
		wants = wants || (r.Wants && r.Host == h)
	}
	if !header || !wants {
		t.Fatal("a remote gets its heading, and its callers join the needs-you section")
	}
	if rows[0].Kind != RowHeader || rows[0].Title != "needs you" {
		t.Fatal("whoever calls, on any host, comes first")
	}
}

func TestAnUnansweredRemoteStillHasItsHeading(t *testing.T) {
	rows := BuildAll(fixture(), nil, []*Host{{Name: "karif"}}, TreeOptions{})
	last := rows[len(rows)-1]
	if last.Kind != RowHeader || last.Host == nil {
		t.Fatal("the heading says it is connecting")
	}
}

func TestTheSelectionComesFirstAmongARemotesPlaces(t *testing.T) {
	sel := fixture().Pane("%3")
	got := withSelected([]Place{{Dir: "/a"}, {Dir: sel.Dir(), Why: "pane", Repo: true}}, sel)
	if len(got) != 2 || got[0].Why != "selected" || !got[0].Repo || got[1].Dir != "/a" {
		t.Fatalf("got %+v", got)
	}
}

// fakeTsm is a "remote" whose tsm is a shell script printing answer.
func fakeTsm(t *testing.T, answer string) Remote {
	dir := t.TempDir()
	bin := filepath.Join(dir, "tsm")
	os.WriteFile(bin, []byte("#!/bin/sh\ncat <<'EOF'\n"+answer+"\nEOF\n"), 0o755)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	return Remote{Conf: RemoteConf{Name: "fake", TSM: bin, Command: []string{"env"}}}
}

func TestAnswersAreCachedAndVersionsChecked(t *testing.T) {
	good := `{"proto":1,"host":"k","snapshot":{"sessions":[{"id":"$1","name":"s","windows":[{"id":"@1","index":1,"name":"w","panes":[{"id":"%1","index":0}]}]}]}}`
	r := fakeTsm(t, good)
	p, err := r.Fetch()
	if err != nil {
		t.Fatal(err)
	}
	if p.Snap.Pane("%1").Window.Session.Name != "s" {
		t.Fatal("back-pointers are restored")
	}
	if c := r.Cached(); c == nil || c.Snap.Pane("%1") == nil {
		t.Fatal("what came back is kept for next time")
	}
	if _, err := fakeTsm(t, `{"proto":2}`).Fetch(); err == nil || !strings.Contains(err.Error(), "proto 2") {
		t.Fatalf("a tsm speaking another proto is named as such: %v", err)
	}
}

func TestRemoteOpErrorsComeBackAsErrors(t *testing.T) {
	r := fakeTsm(t, `{"outcome":{"quit":false},"error":"pane %9 is gone"}`)
	if _, err := r.Do(Op{Action: "kill"}); err == nil || !strings.Contains(err.Error(), "pane %9 is gone") {
		t.Fatalf("err = %v", err)
	}
}

func TestTheControlSocketFitsInAUnixSocketPath(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "/tmp/"+strings.Repeat("long-", 20))
	if p := controlPath(); len(p)-len("%C")+40 > 104 {
		t.Fatalf("%s would be too long once %%C is filled in", p)
	}
}

func TestTheConfigLivesInDotConfigEverywhere(t *testing.T) {
	t.Setenv("TSM_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	if got := configPath(); got != filepath.Join(home, ".config", "tsm", "config.toml") {
		t.Fatalf("got %s", got)
	}
	t.Setenv("XDG_CONFIG_HOME", "/x")
	if got := configPath(); got != "/x/tsm/config.toml" {
		t.Fatalf("got %s", got)
	}
	t.Setenv("TSM_CONFIG", "/y.toml")
	if got := configPath(); got != "/y.toml" {
		t.Fatalf("got %s", got)
	}
}
