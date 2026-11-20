package ais

import "errors"

// What internal/flow needs from ais. ais itself reports problems by dying;
// these wrap that into ordinary errors, so flow can say what it was doing.

// Home is ais's state directory ($AIS_HOME, default ~/.ais).
func Home() string { return aisHome }

// Config reads config.toml (writing the starter one if there is none).
func Config() (cfg map[string]any, err error) {
	err = catch(func() { cfg = loadConfig() })
	return
}

// TakesPrompt is whether profile can be given a first message, and the
// command it runs, for messages that say which profile was meant.
func TakesPrompt(cfg map[string]any, profile string) (ok bool, err error) {
	err = catch(func() { ok = len(harnessSettings(config(cfg), profile).Prompt) > 0 })
	return
}

// ExpandPath is ~ and $VAR expansion as ais does it in config values.
func ExpandPath(s string) string { return expand(s) }

// RepoRoot is the main checkout owning path, or "".
func RepoRoot(path string) string { return repoRoot(path) }

// Slug is the name ais gives a session described as text.
func Slug(text string) string { return slugify(text) }

func catch(fn func()) (err error) {
	defer func() {
		if e := recover(); e != nil {
			x, ok := e.(exit)
			if !ok {
				panic(e)
			}
			err = errors.New(x.msg)
		}
	}()
	fn()
	return nil
}
