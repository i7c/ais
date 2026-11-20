package ais

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func git(args ...string) result { return run("", "git", args...) }

// repoRoot is the main checkout owning path, or "" when path is not in a
// repo. It asks for the *common* dir, so it answers with the main checkout
// even from inside a worktree — the normal case once a session runs in one.
func repoRoot(path string) string {
	if !isDir(path) {
		return ""
	}
	r := git("-C", path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if !r.ok() {
		return ""
	}
	common := strings.TrimSpace(r.Stdout)
	if common == "" {
		return ""
	}
	return filepath.Dir(common)
}

// worktreeRoot is the repo a new session should branch from, or "" to run
// where we are. requested is -w (true), --no-worktree (false) or neither
// (nil, and worktree.auto decides). A typed -w that cannot be honoured is
// fatal, and fatal here, before the session is recorded; the same wish from
// the config is opportunistic.
func worktreeRoot(requested *bool, auto bool, cwd string) string {
	if requested != nil && !*requested || requested == nil && !auto {
		return ""
	}
	root := repoRoot(cwd)
	if root == "" && requested != nil {
		die("-w needs a git repository; %s is not in one", tilde(cwd))
	}
	return root
}

// repoName is what to call this repo's directory inside a session: from the
// origin URL rather than the directory, so a checkout somebody named
// "mstctl-03" still yields "mstctl" and two sessions agree on the name.
func repoName(root string) string {
	r := git("-C", root, "remote", "get-url", "origin")
	url := ""
	if r.ok() {
		url = strings.TrimSpace(r.Stdout)
	}
	if url != "" {
		url = strings.TrimRight(url, "/")
		url = strings.TrimSuffix(url, ".git")
		name := url[strings.LastIndex(url, "/")+1:]
		name = name[strings.LastIndex(name, ":")+1:]
		if name != "" {
			return name
		}
	}
	return filepath.Base(root)
}

// mainRemote is the remote a repo's default branch comes from: origin when
// there is one; else the remote the checkout's own branch tracks; else the
// first there is.
func mainRemote(root string) string {
	r := git("-C", root, "remote")
	if !r.ok() {
		return ""
	}
	remotes := strings.Fields(r.Stdout)
	if len(remotes) == 0 {
		return ""
	}
	if contains(remotes, "origin") {
		return "origin"
	}
	up := git("-C", root, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	tracked := ""
	if up.ok() {
		tracked, _, _ = strings.Cut(strings.TrimSpace(up.Stdout), "/")
	}
	if contains(remotes, tracked) {
		return tracked
	}
	return remotes[0]
}

func hasRef(root, ref string) bool {
	return git("-C", root, "rev-parse", "--verify", "--quiet", ref+"^{commit}").ok()
}

// remoteDefault is the remote's default branch as a remote-tracking ref,
// e.g. origin/main. <remote>/HEAD is a local note of what the default was
// when last asked, so it is asked again when missing or stale.
func remoteDefault(root, remote string) string {
	for attempt := 0; attempt < 2; attempt++ {
		r := git("-C", root, "symbolic-ref", "--quiet", "refs/remotes/"+remote+"/HEAD")
		ref := ""
		if r.ok() {
			ref = strings.TrimSpace(r.Stdout)
		}
		if ref != "" && hasRef(root, ref) {
			return strings.TrimPrefix(ref, "refs/remotes/")
		}
		if attempt == 0 {
			git("-C", root, "remote", "set-head", remote, "--auto")
		}
	}
	for _, name := range []string{"main", "master"} {
		if hasRef(root, "refs/remotes/"+remote+"/"+name) {
			return remote + "/" + name
		}
	}
	return ""
}

// localDefault is the default branch as the main checkout has it.
func localDefault(root, upstream string) string {
	var names []string
	if upstream != "" {
		_, branch, _ := strings.Cut(upstream, "/")
		names = append(names, branch)
	}
	for _, name := range append(names, "main", "master") {
		if hasRef(root, "refs/heads/"+name) {
			return name
		}
	}
	return ""
}

func shortSHA(root, ref string) string {
	r := git("-C", root, "rev-parse", "--short", ref+"^{commit}")
	if !r.ok() {
		return "?"
	}
	return strings.TrimSpace(r.Stdout)
}

func isAncestor(root, older, newer string) bool {
	return git("-C", root, "merge-base", "--is-ancestor", older, newer).ok()
}

// startPoint is the ref a new worktree starts from under worktree.from.
//
// A mode that has nothing to offer here — origin in a repo with no remote,
// local with no default branch — falls back to the next best thing and says
// so, rather than failing: from is set once in the config for every repo,
// and not every repo has every kind of base.
func startPoint(root, mode string) string {
	remote := mainRemote(root)
	upstream := ""
	if remote != "" {
		upstream = remoteDefault(root, remote)
	}
	local := localDefault(root, upstream)

	if mode == "auto" && upstream != "" && local != "" {
		if isAncestor(root, local, upstream) {
			return upstream
		}
		if isAncestor(root, upstream, local) {
			ahead := strings.TrimSpace(git("-C", root, "rev-list", "--count", upstream+".."+local).Stdout)
			note("%s has %s commit(s) not on %s yet; starting from %s", local, ahead, upstream, local)
			return local
		}
		note("%s and %s have diverged; starting from %s", local, upstream, upstream)
		return upstream
	}
	if (mode == "auto" || mode == "origin") && upstream != "" {
		return upstream
	}
	if (mode == "auto" || mode == "origin" || mode == "local") && local != "" {
		if mode == "origin" {
			note("no remote default branch in %s; starting from %s", tilde(root), local)
		}
		return local
	}
	if hasRef(root, "HEAD") {
		if mode != "head" {
			note("no default branch in %s; starting from what it has checked out", tilde(root))
		}
		return "HEAD"
	}
	die("cannot work out what to branch from in %s (no commits)", tilde(root))
	return ""
}

// isWorktree: .git is a dir (main checkout) or a file (worktree).
func isWorktree(path string) bool { return exists(filepath.Join(path, ".git")) }

func worktreeBranch(path string) string {
	r := git("-C", path, "branch", "--show-current")
	if b := strings.TrimSpace(r.Stdout); r.ok() && b != "" {
		return b
	}
	return "(detached)"
}

func isDirty(path string) bool {
	r := git("-C", path, "status", "--porcelain")
	return r.ok() && strings.TrimSpace(r.Stdout) != ""
}

// unpushedCount is commits on HEAD that exist on no remote — unsaved work, in
// the sense that matters when deciding whether a worktree may be thrown away.
func unpushedCount(path string) int {
	r := git("-C", path, "rev-list", "--count", "HEAD", "--not", "--remotes")
	if !r.ok() {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(r.Stdout))
	return n
}

// copyInto carries the untracked files a checkout needs (.env and friends).
func copyInto(root, dest string, paths []string) {
	for _, rel := range paths {
		src := filepath.Join(root, rel)
		if !exists(src) {
			continue
		}
		target := filepath.Join(dest, rel)
		must(os.MkdirAll(filepath.Dir(target), 0o755))
		must(copyTree(src, target))
		note("copied %s", rel)
	}
}

func copyTree(src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return copyFile(src, dst, st.Mode())
	}
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		}
		return copyFile(path, target, info.Mode())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if st, err := in.Stat(); err == nil {
		defer os.Chtimes(dst, st.ModTime(), st.ModTime())
	}
	return out.Close()
}

// ensureWorktree is the worktree at dest, on branch, made if not there yet.
//
// An existing worktree is reused rather than refused: a restart, a second
// "ais wt add" for the same repo and a re-run of "-w" all mean "put me where
// that session works", and the answer to all three is the same directory.
func ensureWorktree(root, dest, branch string, wt wtSettings) string {
	if exists(dest) {
		if isWorktree(dest) {
			note("reusing %s (%s)", tilde(dest), worktreeBranch(dest))
			return dest
		}
		die("%s exists and is not a git worktree; move it aside", tilde(dest))
	}
	remote := mainRemote(root)
	if wt.Fetch && remote != "" {
		if r := git("-C", root, "fetch", "--quiet", remote); !r.ok() {
			lines := strings.Split(strings.TrimSpace(r.Stderr), "\n")
			why := lines[len(lines)-1]
			if why == "" {
				why = "unknown error"
			}
			note("could not fetch %s, so %s/* may be out of date: %s", remote, remote, why)
		}
	}
	must(os.MkdirAll(filepath.Dir(dest), 0o755))
	mode := wt.From

	var r result
	switch {
	case wt.Detach:
		ref := startPoint(root, mode)
		note("detached at %s @ %s", ref, shortSHA(root, ref))
		r = git("-C", root, "worktree", "add", "--detach", dest, ref)
	case hasRef(root, "refs/heads/"+branch):
		note("reusing existing local branch %s @ %s; from=%s does not apply", branch, shortSHA(root, branch), mode)
		r = git("-C", root, "worktree", "add", dest, branch)
	case remote != "" && hasRef(root, "refs/remotes/"+remote+"/"+branch):
		note("tracking existing %s/%s @ %s", remote, branch, shortSHA(root, remote+"/"+branch))
		r = git("-C", root, "worktree", "add", "--track", "-b", branch, dest, remote+"/"+branch)
	default:
		ref := startPoint(root, mode)
		note("new branch %s from %s @ %s", branch, ref, shortSHA(root, ref))
		r = git("-C", root, "worktree", "add", "-b", branch, dest, ref)
	}
	if !r.ok() {
		msg := strings.TrimSpace(r.Stderr)
		if msg == "" {
			msg = "unknown error"
		}
		die("git worktree add failed: %s", msg)
	}
	copyInto(root, dest, wt.Copy)
	return dest
}

// sessionDirname is the single path segment a session's worktrees live
// under: the session name, which is also the branch name; a session with no
// description falls back to its id.
func sessionDirname(r *Record) string {
	if r.Slug != "" {
		return r.Slug
	}
	return r.ID
}

// sessionWorktreeBase is where this session's worktrees go — recorded if it
// has any, else derived.
func sessionWorktreeBase(r *Record, wt wtSettings) string {
	if b := r.wt().Base; b != "" {
		return b
	}
	return filepath.Join(wt.Base, sessionDirname(r))
}

// recordWorktree remembers a session's worktree directory, and its initial
// worktree if any.
func recordWorktree(id, base, initial, branch, root string) bool {
	return update(id, func(r *Record) {
		rec := r.wt()
		rec.Base, rec.Branch = base, branch
		if initial != "" {
			rec.Initial = initial
			r.Cwd = initial
		}
		if root != "" {
			rec.Root = root
		}
		r.Worktree = &rec
	})
}

// sessionWorktrees is every checkout sitting directly under a session dir.
func sessionWorktrees(base string) []string {
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		p := filepath.Join(base, e.Name())
		if isDir(p) && isWorktree(p) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func worktreeState(path string) string {
	var bits []string
	if isDirty(path) {
		bits = append(bits, "dirty")
	}
	if n := unpushedCount(path); n > 0 {
		bits = append(bits, "+"+strconv.Itoa(n)+" unpushed")
	}
	return strings.Join(bits, ", ")
}

// removeWorktree drops a worktree and its branch, keeping anything git is
// unsure about. Only ever called on a worktree found to hold nothing unsaved;
// the branch is a separate judgement, and git's "-d refuses unmerged" is the
// one that counts.
func removeWorktree(path string) bool {
	root := repoRoot(path)
	if root == "" {
		os.RemoveAll(path)
		return true
	}
	branch := worktreeBranch(path)
	r := git("-C", root, "worktree", "remove", path)
	if !r.ok() {
		msg := strings.TrimSpace(r.Stderr)
		if msg == "" {
			msg = "git declined"
		}
		note("could not remove %s: %s", tilde(path), msg)
		return false
	}
	if branch != "(detached)" {
		git("-C", root, "branch", "-d", branch)
	}
	return true
}

// subdirs lists the directories directly under dir, sorted.
func subdirs(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if p := filepath.Join(dir, e.Name()); isDir(p) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func resolveLinks(path string) (string, error) { return filepath.EvalSymlinks(path) }
