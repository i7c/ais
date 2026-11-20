package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func refState() *AisState {
	st := &AisState{API: 1, Host: "host-a", Profiles: []AisProfile{{Name: "pi"}}}
	st.Worktree.Auto, st.Worktree.From = true, "auto"
	st.Worktree.Fetch, st.Worktree.SupportsRefs = true, true
	return st
}

func TestRefFormAcceptsTextAndFetchOverrides(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	snap := fixture()
	f := newCreateForm(snap, refState(), snap.Pane("%1"), true,
		[]Place{{Dir: t.TempDir(), Repo: true}})
	f.focus = fFrom
	f.cycle(-1) // auto -> ref
	if f.from != wtRef || !f.applies(fRef) || !f.applies(fFetch) {
		t.Fatal("ref exposes its text and fetch fields")
	}
	if _, err := f.Spec(); err == nil {
		t.Fatal("an empty custom ref cannot launch")
	}
	f.moveFocus(1)
	f.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("origin/foo-bar")})
	if f.focus != fRef || f.ref.Value() != "origin/foo-bar" {
		t.Fatal("the ref field takes text, including navigation letters")
	}
	f.moveFocus(1)
	f.cycle(1)
	spec, err := f.Spec()
	if err != nil || spec.From != "origin/foo-bar" || spec.Fetch == nil || *spec.Fetch {
		t.Fatalf("custom ref and no-fetch: %+v, %v", spec, err)
	}
	want := "ais run pi -w --from origin/foo-bar --no-fetch"
	if got := ShellJoin(spec.Argv("ais")); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	view := ansi.Strip(strings.Join(f.view(120, 35, "ais"), "\n"))
	for _, text := range []string{"origin/foo-bar", "fetch", "--no-fetch", "detached"} {
		if !strings.Contains(view, text) {
			t.Fatalf("preview lacks %q: %s", text, view)
		}
	}
	f.worktree = false
	if f.applies(fRef) || f.applies(fFetch) {
		t.Fatal("ref controls hide without a new worktree")
	}
	if spec, err = f.Spec(); err != nil || spec.From != "" || spec.Fetch != nil {
		t.Fatalf("hidden fields never emit creation flags: %+v, %v", spec, err)
	}
}

func TestRefValidationAndReservedModes(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	f := newCreateForm(fixture(), refState(), nil, true, []Place{{Dir: t.TempDir(), Repo: true}})
	f.from = wtRef
	for _, ref := range []string{"", " ", "--help", " HEAD", "auto", "head", "origin", "local"} {
		f.ref.SetValue(ref)
		if _, err := f.Spec(); err == nil {
			t.Errorf("accepted %q as a custom ref", ref)
		}
	}
	for _, ref := range []string{"refs/heads/head", "ref", "v1.0", "HEAD~2", "abc1234"} {
		f.ref.SetValue(ref)
		if spec, err := f.Spec(); err != nil || spec.From != ref {
			t.Errorf("%q: %+v, %v", ref, spec, err)
		}
	}
}

func TestConfiguredRefIsAFormDefault(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	st := refState()
	st.Worktree.From, st.Worktree.Fetch = "origin/configured", false
	f := newCreateForm(fixture(), st, nil, true, []Place{{Dir: t.TempDir(), Repo: true}})
	if f.from != wtRef || f.ref.Value() != st.Worktree.From || f.fetch {
		t.Fatalf("not initialized from host config: %+v", f)
	}
	if spec, err := f.Spec(); err != nil || spec.From != "" || spec.Fetch != nil {
		t.Fatalf("unchanged defaults need no flags: %+v, %v", spec, err)
	}
	f.focus = fFetch
	f.cycle(1)
	if spec, err := f.Spec(); err != nil || spec.Fetch == nil || !*spec.Fetch {
		t.Fatalf("fetch can override a disabled config: %+v, %v", spec, err)
	}
	st.Worktree.From = "ref" // a branch named ref is not an ais mode
	f = newCreateForm(fixture(), st, nil, true, []Place{{Dir: t.TempDir(), Repo: true}})
	if f.from != wtRef || f.ref.Value() != "ref" {
		t.Fatal("the UI label ref must not hide a configured branch named ref")
	}
}

func TestCustomRefsAreRememberedOnlyForTheirHostAndDirectory(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	st := refState()
	a, b := Place{Dir: t.TempDir(), Repo: true}, Place{Dir: t.TempDir(), Repo: true}
	f := newCreateForm(fixture(), st, nil, true, []Place{a, b})
	f.from, f.preferRef = wtRef, true
	f.ref.SetValue("origin/only-a")
	f.fetch = false
	f.remember()

	for _, tc := range []struct {
		host  string
		place Place
		want  bool
	}{{"host-a", a, true}, {"host-a", b, false}, {"host-b", a, false}} {
		st.Host = tc.host
		form := newCreateForm(fixture(), st, nil, true, []Place{tc.place})
		if tc.want {
			if form.from != wtRef || form.ref.Value() != "origin/only-a" || form.fetch {
				t.Fatal("same repo and host did not restore the custom choice")
			}
		} else if form.from == wtRef || form.ref.Value() != "" || !form.fetch {
			t.Fatal("custom ref or fetch setting leaked to another repo/host")
		}
	}

	// Changing the selected repo inside one form must also clear/restore refs.
	f.focus = fWhere
	f.update(tea.KeyMsg{Type: tea.KeyDown})
	if f.from == wtRef || f.ref.Value() != "" || !f.fetch {
		t.Fatal("changing the directory leaked the previous custom ref")
	}
	f.update(tea.KeyMsg{Type: tea.KeyUp})
	if f.from != wtRef || f.ref.Value() != "origin/only-a" || f.fetch {
		t.Fatal("returning to the directory did not restore its choice")
	}
}

func TestOldAisNeverGetsRefOrFetchOptions(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	st := refState()
	st.Worktree.SupportsRefs = false
	f := newCreateForm(fixture(), st, nil, true, []Place{{Dir: t.TempDir(), Repo: true}})
	f.focus = fFrom
	for range 8 {
		f.cycle(1)
		if f.from == wtRef || f.applies(fRef) || f.applies(fFetch) {
			t.Fatal("old ais must not offer ref controls")
		}
		if spec, err := f.Spec(); err != nil || spec.Fetch != nil {
			t.Fatalf("old ais gets only old options: %+v, %v", spec, err)
		}
	}
	view := ansi.Strip(strings.Join(f.view(120, 35, "ais"), "\n"))
	if strings.Contains(view, " ref ") {
		t.Fatal("ref choice rendered for an old ais")
	}
}

func TestRefAndFetchSurviveRemoteWireAndExecution(t *testing.T) {
	for _, fetch := range []bool{false, true} {
		c := CreateSpec{Profile: "pi", Dir: t.TempDir(), Worktree: true,
			From: "origin/foo-bar", Fetch: &fetch, Place: NewWindow, Anchor: fixture().Pane("%1")}
		data, err := json.Marshal(Op{Action: "create", Create: c.Wire()})
		if err != nil {
			t.Fatal(err)
		}
		var op Op
		if err := json.Unmarshal(data, &op); err != nil {
			t.Fatal(err)
		}
		if op.Create.Fetch == nil || *op.Create.Fetch != fetch || op.Create.From != c.From {
			t.Fatalf("wire lost choices: %s", data)
		}
		r := &fakeRunner{reply: func(a []string) string { return "%9\n" }}
		if _, err := Exec(actions(r), nil, op); err != nil {
			t.Fatal(err)
		}
		typed := r.find("send-keys")
		if typed == nil || typed[len(typed)-1] != ShellJoin(c.Argv("ais")) {
			t.Fatalf("destination lost choices: %v", r.calls)
		}
	}
}

func TestFetchTriStateAndRefShellQuoting(t *testing.T) {
	yes, no := true, false
	for _, fetch := range []*bool{nil, &yes, &no} {
		c := CreateSpec{Profile: "pi", Worktree: true, From: "HEAD@{yesterday}", Fetch: fetch}
		argv := c.Argv("ais")
		want := []string{"ais", "run", "pi", "-w", "--from", c.From}
		if fetch != nil && *fetch {
			want = append(want, "--fetch")
		} else if fetch != nil {
			want = append(want, "--no-fetch")
		}
		if !reflect.DeepEqual(argv, want) {
			t.Fatalf("%v != %v", argv, want)
		}
		c.From = "$(touch /tmp/not-executed)"
		if !strings.Contains(ShellJoin(c.Argv("ais")), "'"+c.From+"'") {
			t.Fatal("Git refs must be shell-quoted just like other arguments")
		}
		c.Worktree = false
		if got := ShellJoin(c.Argv("ais")); got != "ais run pi --no-worktree" {
			t.Fatal(got)
		}
	}
}

func TestRefCapabilityIsAdditiveInAisState(t *testing.T) {
	st, err := ParseAisState([]byte(`{"api":1,"worktree":{"from":"origin/feature","fetch":true,"supports_refs":true}}`))
	if err != nil || !st.Worktree.SupportsRefs || !st.Worktree.Fetch || st.Worktree.From != "origin/feature" {
		t.Fatalf("state %+v, %v", st, err)
	}
	old, err := ParseAisState([]byte(`{"api":1,"worktree":{"from":"auto"}}`))
	if err != nil || old.Worktree.SupportsRefs {
		t.Fatal("absent capability means old ais, not implicit support")
	}
}
