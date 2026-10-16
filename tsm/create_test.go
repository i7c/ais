package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTheCommandSaysWhatWasChosen(t *testing.T) {
	c := CreateSpec{Profile: "claude", Worktree: true, About: "port the parser"}
	if got := ShellJoin(c.Argv("ais")); got != "ais run claude -w -d 'port the parser'" {
		t.Fatal(got)
	}
	c.Worktree = false
	if got := ShellJoin(c.Argv("ais")); got != "ais run claude --no-worktree -d 'port the parser'" {
		t.Fatal("here means here, whatever worktree.auto says: " + got)
	}
	if (CreateSpec{Profile: shellProfile}).Argv("ais") != nil {
		t.Fatal("a shell needs nothing typed into it")
	}
}

func TestNewSessionsAreNamedLikeTheirWork(t *testing.T) {
	r := &fakeRunner{reply: func(a []string) string {
		if a[0] == "name" {
			return "port-parser\n" // what ais makes of the description
		}
		return ""
	}}
	acts := actions(r)
	if got := acts.sessionName(CreateSpec{Profile: "claude", About: "port the parser"}); got != "port-parser" {
		t.Fatal(got)
	}
	if got := acts.sessionName(CreateSpec{Profile: shellProfile, About: "Scratch: notes"}); got != "scratch-notes" {
		t.Fatal(got)
	}
	if got := acts.sessionName(CreateSpec{Profile: shellProfile, Dir: "/x/soi"}); got != "soi-2" {
		t.Fatal("a name that is taken gets a number: " + got)
	}
}

func TestCreatePutsItWhereItWasAsked(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		place Placement
		cmd   string
	}{{NewWindow, "new-window"}, {SplitRight, "split-window"}, {NewSession, "new-session"}} {
		r := &fakeRunner{reply: func(a []string) string { return "%9\n" }}
		acts := actions(r)
		_, err := acts.Create(CreateSpec{Profile: "claude", Dir: dir, Place: tc.place, Anchor: acts.snap.Pane("%1")})
		if err != nil {
			t.Fatal(err)
		}
		if made := r.find(tc.cmd); made == nil || !strings.Contains(strings.Join(made, " "), "-c "+dir) {
			t.Fatalf("%s: %v", tc.cmd, r.calls)
		}
		if typed := r.find("send-keys"); typed == nil || !strings.HasPrefix(typed[len(typed)-1], "ais run claude") {
			t.Fatalf("nothing typed: %v", r.calls)
		}
	}
}

func mkrepo(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReposAreFoundTwoLevelsDownButNotInsideRepos(t *testing.T) {
	root := t.TempDir()
	a := mkrepo(t, filepath.Join(root, "a"))
	mkrepo(t, filepath.Join(a, "vendored")) // inside a repo: not offered
	b := mkrepo(t, filepath.Join(root, "group", "b"))
	mkrepo(t, filepath.Join(root, "x", "y", "too-deep"))
	if got := repos([]string{root}); !reflect.DeepEqual(got, []string{a, b}) {
		t.Fatalf("repos = %v", got)
	}
}

func TestPlacesComeMostLikelyFirstAndOnce(t *testing.T) {
	root := t.TempDir()
	repo := mkrepo(t, filepath.Join(root, "git", "repo"))
	wtBase := filepath.Join(root, "wt")
	wt := mkrepo(t, filepath.Join(wtBase, "fix-it", "repo"))
	other := filepath.Join(root, "other")
	os.MkdirAll(other, 0o755)

	out := strings.Join([]string{
		line("$1", "s", "@1", 1, "w", "%1", 1, "zsh", other),
		line("$1", "s", "@1", 1, "w", "%2", 2, "zsh", repo),
		clientMarker + strings.Join([]string{"/dev/pts/1", "$1", "%1", "1"}, sep),
	}, "\n")
	snap := ParseSnapshot(out)
	st := &AisState{API: 1, Host: "h"}
	st.Worktree.Base = wtBase
	st.Sessions = []AisSession{{ID: "x", Host: "h", Cwd: wt}}

	places := Places(snap, st, snap.Pane("%2"), []string{filepath.Join(root, "git")})
	var got []string
	for _, p := range places {
		got = append(got, p.Why+":"+filepath.Base(filepath.Dir(p.Dir))+"/"+filepath.Base(p.Dir))
	}
	want := []string{"selected:git/repo", "here:" + filepath.Base(root) + "/other", "agent:fix-it/repo"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("places = %v", got)
	}
	if !places[0].Repo || places[1].Repo {
		t.Fatal("repos are told apart")
	}
}

func TestATypedPathIsOfferedFirst(t *testing.T) {
	dir := t.TempDir()
	got := FilterPlaces([]Place{{Dir: "/elsewhere"}}, dir)
	if len(got) != 1 || got[0].Dir != dir || got[0].Why != "typed" {
		t.Fatalf("got %+v", got)
	}
	if got := FilterPlaces([]Place{{Dir: dir}}, dir); len(got) != 1 || got[0].Why == "typed" {
		t.Fatal("a typed path already on the list is not offered twice")
	}
}

func TestTheFormRemembersTheLastChoices(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	snap := fixture()
	st := &AisState{API: 1, Profiles: []AisProfile{{Name: "claude"}, {Name: "pi"}}}
	st.Worktree.Auto = true

	f := newCreateForm(snap, st, snap.Pane("%1"), true)
	if f.profile() != "claude" || !f.worktree || placements[f.place] != NewWindow {
		t.Fatal("first time: the first profile, worktree.auto, a new window")
	}
	f.what, f.place = 1, 3
	f.remember()

	f = newCreateForm(snap, st, snap.Pane("%1"), true)
	if f.profile() != "pi" || placements[f.place] != NewSession {
		t.Fatalf("second time: what was chosen, got %q %v", f.profile(), placements[f.place])
	}
	if f := newCreateForm(snap, st, nil, true); placements[f.place] != NewSession {
		t.Fatal("with nothing to put it beside, a new session is the only place")
	}
	if f := newCreateForm(snap, nil, nil, false); !reflect.DeepEqual(f.profiles, []string{shellProfile}) {
		t.Fatal("without ais there is only the shell")
	}
}
