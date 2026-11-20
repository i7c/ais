package ais

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"syscall"
)

// Record is one session in sessions.json (version 1). Keys this version of
// ais does not know are kept in Extra and written back as they were: the file
// outlives every binary that writes it, older and newer alike.
type Record struct {
	ID          string            `json:"id"`
	Description *string           `json:"description"`
	Profile     string            `json:"profile,omitempty"`
	Harness     string            `json:"harness,omitempty"` // the profile's old name, still read by older ais
	Command     []string          `json:"command"`
	HarnessArgs *[]string         `json:"harness_args,omitempty"` // what restart replays; nil in old records
	SessionIDs  []string          `json:"session_ids"`
	Status      string            `json:"status"`
	PID         *int              `json:"pid"`
	LauncherPID int               `json:"launcher_pid,omitempty"`
	Hostname    string            `json:"hostname"`
	Cwd         string            `json:"cwd"`
	Tmux        map[string]string `json:"tmux"`
	StartedAt   string            `json:"started_at"`
	EndedAt     *string           `json:"ended_at"`
	ExitCode    *int              `json:"exit_code"`
	Slug        string            `json:"slug,omitempty"`
	Worktree    *WorktreeRec      `json:"worktree,omitempty"`
	RestartOf   string            `json:"restart_of,omitempty"`
	ForkOf      string            `json:"fork_of,omitempty"`
	Reaped      bool              `json:"reaped,omitempty"`
	// The first prompt ais gave the harness, so that its own UserPromptSubmit
	// is not mistaken for you typing (see cmdActivity).
	InitialPrompt     string `json:"initial_prompt,omitempty"`
	InitialPromptSeen bool   `json:"initial_prompt_seen,omitempty"`
	LastInputAt       string `json:"last_input_at,omitempty"` // when you last sent the harness a prompt
	FlowProposal      string `json:"flow_proposal,omitempty"` // the ais flow proposal this session works on
	FlowPlanner       string `json:"flow_planner,omitempty"`  // the ais flow planner this session is
	Notes             []Note `json:"notifications,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

type WorktreeRec struct {
	Base    string `json:"base,omitempty"`
	Initial string `json:"initial,omitempty"`
	Branch  string `json:"branch,omitempty"`
	Root    string `json:"root,omitempty"`
}

// Note is one notification on a record.
type Note struct {
	ID         string `json:"id,omitempty"`
	At         string `json:"at"`
	Message    string `json:"message"`
	Delivered  bool   `json:"delivered"`
	Suppressed bool   `json:"suppressed"`
	Via        string `json:"via,omitempty"`
	Error      string `json:"error,omitempty"`
	SeenAt     string `json:"seen_at,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

func (r *Record) UnmarshalJSON(b []byte) error {
	type plain Record
	if err := json.Unmarshal(b, (*plain)(r)); err != nil {
		return err
	}
	return extras(b, r, &r.Extra)
}

func (r Record) MarshalJSON() ([]byte, error) {
	type plain Record
	return withExtras(plain(r), r.Extra)
}

func (n *Note) UnmarshalJSON(b []byte) error {
	type plain Note
	if err := json.Unmarshal(b, (*plain)(n)); err != nil {
		return err
	}
	return extras(b, n, &n.Extra)
}

func (n Note) MarshalJSON() ([]byte, error) {
	type plain Note
	return withExtras(plain(n), n.Extra)
}

// extras collects the keys of b that known does not name.
func extras(b []byte, known any, out *map[string]json.RawMessage) error {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	names := knownKeys(known)
	for k, v := range all {
		if !names[k] {
			if *out == nil {
				*out = map[string]json.RawMessage{}
			}
			(*out)[k] = v
		}
	}
	return nil
}

func knownKeys(v any) map[string]bool {
	// Every field of these types is tagged, so the empty value marshals with
	// all its non-omitempty keys; omitempty ones are listed by hand below.
	names := map[string]bool{}
	switch v.(type) {
	case *Record:
		for _, k := range []string{"id", "description", "profile", "harness", "command", "harness_args",
			"session_ids", "status", "pid", "launcher_pid", "hostname", "cwd", "tmux", "started_at",
			"ended_at", "exit_code", "slug", "worktree", "restart_of", "fork_of", "reaped", "notifications",
			"initial_prompt", "initial_prompt_seen", "last_input_at", "flow_proposal", "flow_planner"} {
			names[k] = true
		}
	case *Note:
		for _, k := range []string{"id", "at", "message", "delivered", "suppressed", "via", "error", "seen_at"} {
			names[k] = true
		}
	}
	return names
}

func withExtras(v any, extra map[string]json.RawMessage) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil || len(extra) == 0 {
		return b, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	for k, raw := range extra {
		if _, known := m[k]; !known {
			m[k] = raw
		}
	}
	return json.Marshal(m)
}

// Helpers mirroring the Python's session.get(...) idioms.

func (r *Record) desc() string {
	if r.Description == nil {
		return ""
	}
	return *r.Description
}

func (r *Record) profile() string {
	if r.Profile != "" {
		return r.Profile
	}
	return r.Harness
}

func (r *Record) harness() string {
	if r.Harness != "" {
		return r.Harness
	}
	return r.Profile
}

func (r *Record) pid() int {
	if r.PID == nil {
		return 0
	}
	return *r.PID
}

func (r *Record) conversation() string {
	if len(r.SessionIDs) == 0 {
		return ""
	}
	return r.SessionIDs[len(r.SessionIDs)-1]
}

func (r *Record) wt() WorktreeRec {
	if r.Worktree == nil {
		return WorktreeRec{}
	}
	return *r.Worktree
}

func (r *Record) lastNote() *Note {
	if len(r.Notes) == 0 {
		return nil
	}
	return &r.Notes[len(r.Notes)-1]
}

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }

// ---------------------------------------------------------------- the file

const sessionsVersion = 1

func readSessions() []*Record {
	b, err := os.ReadFile(sessionPath)
	if err != nil {
		return nil
	}
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return nil
	}
	// Either {"version": 1, "sessions": [...]} or, from very old ais, a bare list.
	var wrapped struct {
		Sessions []*Record `json:"sessions"`
	}
	if b[0] == '[' {
		if err := json.Unmarshal(b, &wrapped.Sessions); err != nil {
			die("%s is not valid JSON (%v); move it aside to start over", tilde(sessionPath), err)
		}
	} else if err := json.Unmarshal(b, &wrapped); err != nil {
		die("%s is not valid JSON (%v); move it aside to start over", tilde(sessionPath), err)
	}
	return wrapped.Sessions
}

func writeSessions(sessions []*Record) {
	must(os.MkdirAll(aisHome, 0o755))
	if sessions == nil {
		sessions = []*Record{}
	}
	payload := struct {
		Version  int       `json:"version"`
		Sessions []*Record `json:"sessions"`
	}{sessionsVersion, sessions}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	must(enc.Encode(payload))
	tmp := filepath.Join(filepath.Dir(sessionPath), "sessions.json.tmp")
	must(os.WriteFile(tmp, buf.Bytes(), 0o644))
	must(os.Rename(tmp, sessionPath)) // atomic: readers never see a half file
}

// mutate applies fn to the session list under the lock and writes it back.
// fn works on the slice in place; it may replace it by returning a new one.
func mutate(fn func(sessions []*Record) []*Record) []*Record {
	must(os.MkdirAll(aisHome, 0o755))
	fd, err := syscall.Open(lockPath, syscall.O_CREAT|syscall.O_RDWR, 0o600)
	if err != nil {
		die("cannot open %s: %v", tilde(lockPath), err)
	}
	defer syscall.Close(fd)
	must(syscall.Flock(fd, syscall.LOCK_EX))
	defer syscall.Flock(fd, syscall.LOCK_UN)
	sessions := readSessions()
	sessions = fn(sessions)
	writeSessions(sessions)
	return sessions
}

// update applies fn to the session with id under the lock; false when there
// is no such session.
func update(id string, fn func(r *Record)) bool {
	found := false
	mutate(func(ss []*Record) []*Record {
		if r := findID(ss, id); r != nil {
			fn(r)
			found = true
		}
		return ss
	})
	return found
}

func findID(ss []*Record, id string) *Record {
	for _, s := range ss {
		if s.ID == id {
			return s
		}
	}
	return nil
}

func reapedSessions() []*Record {
	return mutate(func(ss []*Record) []*Record { reap(ss); return ss })
}

// ---------------------------------------------------------------- ids

const idAlphabet = "23456789abcdefghijkmnpqrstuvwxyz"
const idLength = 10

func randomString(n int) string {
	out := make([]byte, n)
	for i := range out {
		k, err := rand.Int(rand.Reader, big.NewInt(int64(len(idAlphabet))))
		must(err)
		out[i] = idAlphabet[k.Int64()]
	}
	return string(out)
}

func newID(ss []*Record) string {
	for {
		id := randomString(idLength)
		if findID(ss, id) == nil {
			return id
		}
	}
}

func uuid4() string {
	var b [16]byte
	_, err := rand.Read(b[:])
	must(err)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	const hex = "0123456789abcdef"
	out := make([]byte, 0, 36)
	for i, c := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out = append(out, '-')
		}
		out = append(out, hex[c>>4], hex[c&0xf])
	}
	return string(out)
}
