package ais

import (
	"fmt"
	"os"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

const (
	slugWords = 4
	slugMax   = 32
)

// slugify kebab-cases a description down to the few words that identify it.
// Deterministic on purpose: the same description must always yield the same
// name, whoever asks and however many times.
func slugify(text string) string {
	var ascii strings.Builder
	for _, r := range norm.NFKD.String(text) {
		if r <= unicode.MaxASCII {
			ascii.WriteRune(r)
		}
	}
	words := strings.FieldsFunc(strings.ToLower(ascii.String()), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	var kept []string
	for _, w := range words {
		if !stopwords[w] {
			kept = append(kept, w)
		}
	}
	if len(kept) == 0 {
		kept = words
	}
	var out []string
	for i, word := range kept {
		if i >= slugWords {
			break
		}
		if len(out) > 0 && len(strings.Join(out, "-"))+1+len(word) > slugMax {
			break // stop at a word boundary rather than truncating mid-word
		}
		out = append(out, word)
	}
	slug := strings.Join(out, "-")
	if len(slug) > slugMax {
		slug = slug[:slugMax]
	}
	return strings.Trim(slug, "-")
}

// cmdName prints the session base name for worktrees, branches and the like:
// derived once from the description, then persisted, so every later call in
// this session — in any repo — returns the same name.
func cmdName(set, of *string) int {
	if of != nil {
		// The name a session described this way would get, for callers that
		// want to name things to match before the session exists. Not stored.
		name := slugify(*of)
		if name == "" {
			return 1
		}
		fmt.Println(name)
		return 0
	}
	sid := os.Getenv("AIS_SESSION")
	if sid == "" {
		note("not running inside an ais session; no session name to derive")
		return 1
	}
	if set != nil && *set != "" {
		chosen := slugify(*set)
		if chosen == "" {
			die("%s has no usable characters for a name", pyRepr(*set))
		}
		if storeSlug(sid, chosen, true) == "" {
			note("session %s is not on record; name not persisted", sid)
		}
		fmt.Println(chosen)
		return 0
	}
	existing := findID(readSessions(), sid)
	if existing != nil && existing.Slug != "" {
		fmt.Println(existing.Slug)
		return 0
	}
	description := ""
	if existing != nil {
		description = existing.desc()
	}
	if description == "" {
		description = os.Getenv("AIS_SESSION_DESCRIPTION")
	}
	candidate := orDefault(slugify(description), sid)
	if existing == nil {
		// Session was pruned; still answer, just without remembering.
		fmt.Println(candidate)
		return 0
	}
	fmt.Println(orDefault(storeSlug(sid, candidate, false), candidate))
	return 0
}

// storeSlug persists a name for sid, disambiguating against other sessions'
// names; it returns the stored name, or "" if sid is not on record.
func storeSlug(sid, candidate string, force bool) string {
	stored := ""
	mutate(func(ss []*Record) []*Record {
		target := findID(ss, sid)
		if target == nil {
			return ss
		}
		if target.Slug != "" && !force {
			stored = target.Slug
			return ss
		}
		taken := map[string]bool{}
		for _, s := range ss {
			if s.Slug != "" && s.ID != sid {
				taken[s.Slug] = true
			}
		}
		name := candidate
		if taken[name] {
			// Same description, different session: keep the readable stem
			// and let the id make it unique.
			stem := candidate
			if len(stem) > slugMax-5 {
				stem = stem[:slugMax-5]
			}
			name = strings.TrimRight(stem, "-") + "-" + sid[:4]
		}
		target.Slug = name
		stored = name
		return ss
	})
	return stored
}
