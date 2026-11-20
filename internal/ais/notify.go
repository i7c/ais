package ais

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type notifyArgs struct {
	Message []string
	Title   *string
	Tag     *string
	Force   bool
	NoSound bool
	Quiet   bool
}

// notificationID is unique, and sorting by it is sorting by time:
// microseconds since the epoch in fixed-width hex, then a little noise for
// two in the same microsecond. Fixed width is what makes plain string
// comparison chronological, so "everything after id X" (ais state --since)
// needs no clock parsing on either side.
func notificationID() string {
	return fmt.Sprintf("%014x", time.Now().UnixNano()/1000) + randomString(4)
}

func recordNotification(id string, entry Note) bool {
	return update(id, func(r *Record) {
		items := append(r.Notes, entry)
		var fresh []Note
		for _, n := range items {
			if secs, ok := secondsSince(n.At); !ok || secs <= notifyMaxAge {
				fresh = append(fresh, n)
			}
		}
		if len(fresh) == 0 {
			fresh = items
		}
		if len(fresh) > notifyKeep {
			fresh = fresh[len(fresh)-notifyKeep:]
		}
		r.Notes = fresh
	})
}

func recentNotes(r *Record, windowMinutes float64) []Note {
	var out []Note
	for _, n := range r.Notes {
		if secs, ok := secondsSince(n.At); ok && secs <= windowMinutes*60 {
			out = append(out, n)
		}
	}
	return out
}

// calling is whether the session is waiting for you: its last notification
// is recent and nobody has looked at its pane since (see markSeen).
func calling(r *Record, windowMinutes float64) bool {
	last := r.lastNote()
	return last != nil && last.SeenAt == "" && len(recentNotes(r, windowMinutes)) > 0
}

// notifyTag is the short label that tells parallel sessions apart in a banner.
func notifyTag(r *Record) string {
	if r != nil {
		if tag := strings.TrimSpace(orDefault(r.desc(), r.Slug)); tag != "" {
			return tag
		}
	}
	if tag := strings.TrimSpace(os.Getenv("AIS_SESSION_DESCRIPTION")); tag != "" {
		return tag
	}
	return os.Getenv("AIS_SESSION")
}

func appleScriptString(text string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(text, `\`, `\\`), `"`, `\"`) + `"`
}

func deliverDesktop(title, message, sound string) (bool, string) {
	switch runtime.GOOS {
	case "darwin":
		script := fmt.Sprintf("display notification %s with title %s", appleScriptString(message), appleScriptString(title))
		if sound != "" {
			script += " sound name " + appleScriptString(sound)
		}
		if r := run("", "osascript", "-e", script); !r.ok() {
			return false, orDefault(strings.TrimSpace(r.Stderr), "osascript failed")
		}
		return true, ""
	case "linux":
		if _, err := exec.LookPath("notify-send"); err != nil {
			// Headless Linux gets its own backend later; the record is the fallback.
			return false, "no desktop notifier found (notify-send missing)"
		}
		if r := run("", "notify-send", "--app-name=ais", title, message); !r.ok() {
			return false, orDefault(strings.TrimSpace(r.Stderr), "notify-send failed")
		}
		return true, ""
	}
	return false, "no notification backend for " + runtime.GOOS
}

// offerToSink hands a notification to [notify] sink; what became of it, or
// "" when the sink declined.
//
// The sink reads the notification as JSON on stdin and answers on stdout
// with {"result": "shown"} or {"result": "suppressed"}, exiting 0; anything
// else — a non-zero exit, no answer, no such command — means it declined,
// and ais delivers the notification itself.
func offerToSink(sink []string, payload map[string]any) string {
	data, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, sink[0], sink[1:]...)
	cmd.Stdin = bytes.NewReader(data)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return ""
	}
	var answer struct {
		Result string `json:"result"`
	}
	text := strings.TrimSpace(out.String())
	if text == "" {
		text = "{}"
	}
	if json.Unmarshal([]byte(text), &answer) != nil {
		return ""
	}
	if answer.Result == "shown" || answer.Result == "suppressed" {
		return answer.Result
	}
	return ""
}

// notifyPane is the pane whose being on screen makes a banner pointless:
// the session's own recorded pane — $TMUX_PANE only answers for the shell
// running this command, unless that shell is a sibling of the session.
func notifyPane(r *Record) string {
	own := os.Getenv("TMUX_PANE")
	if os.Getenv("AIS_SIBLING") != "" && own != "" {
		return own
	}
	if r != nil && r.Tmux["pane_id"] != "" {
		return r.Tmux["pane_id"]
	}
	return own
}

// cmdNotify raises a desktop notification for this session and remembers it.
//
// Skipped — recorded, but not shown — while the session's own pane is the
// one on screen. With [notify] sink configured, the sink is asked first and
// decides; it is told whether the pane is on screen here, since "here" may
// not be where you are looking.
func cmdNotify(a notifyArgs) int {
	message := strings.TrimSpace(strings.Join(a.Message, " "))
	if message == "" {
		die(`nothing to say: ais notify "the message"`)
	}
	conf := notifySettings()
	sid := os.Getenv("AIS_SESSION")
	var r *Record
	if sid != "" {
		r = findID(readSessions(), sid)
	}
	pane := notifyPane(r)
	visible := paneIsVisible(pane)
	if r != nil && pane != "" {
		installSeenHooks() // in case a re-sourced tmux.conf took them away
	}
	suppress := visible && conf.WhenVisible == "skip" && !a.Force

	tag := notifyTag(r)
	if a.Tag != nil {
		tag = *a.Tag
	}
	harness := prog
	if r != nil && r.harness() != "" {
		harness = r.harness()
	} else if h := os.Getenv("AIS_HARNESS"); h != "" {
		harness = h
	}
	title := harness
	if tag != "" {
		title = harness + " — " + elide(tag, 40)
	}
	if a.Title != nil && *a.Title != "" {
		title = *a.Title
	}
	sound := conf.Sound
	if a.NoSound {
		sound = ""
	}
	nid, at := notificationID(), nowISO()

	var delivered bool
	var errMsg, via string
	if len(conf.Sink) > 0 {
		result := offerToSink(conf.Sink, map[string]any{
			"id": nid, "at": at, "title": title, "message": message, "tag": tag,
			"session": sid, "harness": harness, "pane": pane,
			"visible": visible, "force": a.Force, "host": hostname(),
		})
		if result != "" {
			via = "sink"
			delivered, suppress = result == "shown", result == "suppressed"
		}
	}
	if via == "" && !suppress {
		delivered, errMsg = deliverDesktop(title, message, sound)
	}

	entry := Note{ID: nid, At: at, Message: message, Delivered: delivered, Suppressed: suppress, Via: via, Error: errMsg}
	if sid != "" {
		if !recordNotification(sid, entry) {
			note("session %s is not on record; notification not remembered", sid)
		}
	} else if !a.Quiet {
		note("not inside an ais session; notification sent but not recorded")
	}
	if errMsg != "" && !a.Quiet {
		note("could not raise a desktop notification: %s", errMsg)
	}
	if suppress && !a.Quiet {
		note("pane is on screen; recorded without a banner (--force to override)")
	}
	if delivered || suppress {
		return 0
	}
	return 1
}
