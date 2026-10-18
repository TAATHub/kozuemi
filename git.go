package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Repo struct {
	Dir string
}

func gitCmd(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// Background commands have no TTY: a credential prompt or editor would hang forever.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=true", "GIT_OPTIONAL_LOCKS=0")
	return cmd
}

func (r Repo) run(args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := gitCmd(r.Dir, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.String(), errors.New(msg)
	}
	return stdout.String(), nil
}

func (r Repo) runCombined(args ...string) (string, error) {
	return r.runEnv(nil, args...)
}

func (r Repo) runEnv(env []string, args ...string) (string, error) {
	cmd := gitCmd(r.Dir, args...)
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		if s == "" {
			s = err.Error()
		}
		return s, errors.New(s)
	}
	return s, nil
}

type RefKind int

const (
	RefHead RefKind = iota
	RefLocal
	RefRemote
	RefTag
)

type Ref struct {
	Name string
	Kind RefKind
}

type Row struct {
	Cells     []gcell
	NodeColor int
	Parents   []string
	Hash      string
	Short     string
	Author    string
	Date      string
	Subject   string
	Refs      []Ref
	// Virtual marks the synthetic "uncommitted changes" row placed above HEAD.
	Virtual bool
}

func (r Row) IsCommit() bool   { return r.Hash != "" }
func (r Row) Selectable() bool { return r.Hash != "" || r.Virtual }

type Tag struct {
	Name    string
	Hash    string
	Date    string
	Subject string
}

type Branch struct {
	Name     string
	Hash     string
	Upstream string
	Track    string
	Worktree string
	Date     string
	Subject  string
	Remote   bool
}

type Worktree struct {
	Path     string
	Hash     string
	Branch   string
	Detached bool
	Bare     bool
	Locked   bool
	Prunable bool
}

func (w Worktree) Name() string { return filepath.Base(w.Path) }

type Head struct {
	Hash     string
	Branch   string
	Upstream string
	Track    string
	State    string
	Toplevel string
	Dirty    int
	Files    map[string]string // path -> porcelain XY (index, worktree)
	Remotes  []string
}

type Snapshot struct {
	Rows      []Row
	Locals    []Branch
	Remotes   []Branch
	Worktrees []Worktree
	Tags      []Tag
	Head      Head
}

func (r Repo) Load(max int) (*Snapshot, error) {
	head, err := r.loadHead()
	if err != nil {
		return nil, err
	}
	locals, remotes, err := r.loadBranches()
	if err != nil {
		return nil, err
	}
	for _, b := range locals {
		if b.Name == head.Branch {
			head.Upstream, head.Track = b.Upstream, b.Track
		}
	}
	wts, err := r.loadWorktrees()
	if err != nil {
		return nil, err
	}
	localSet := map[string]bool{}
	for _, b := range locals {
		localSet[b.Name] = true
	}
	tags, err := r.loadTags()
	if err != nil {
		return nil, err
	}
	rows, err := r.loadGraph(max, localSet, head)
	if err != nil && head.Hash != "" {
		return nil, err
	}
	return &Snapshot{Rows: rows, Locals: locals, Remotes: remotes, Worktrees: wts, Tags: tags, Head: head}, nil
}

func (r Repo) loadTags() ([]Tag, error) {
	out, err := r.run("for-each-ref", "--sort=-creatordate",
		"--format=%(refname:short)%1f%(objectname)%1f%(*objectname)%1f%(creatordate:relative)%1f%(contents:subject)",
		"refs/tags")
	if err != nil {
		return nil, err
	}
	var tags []Tag
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\x1f")
		if len(f) < 5 {
			continue
		}
		t := Tag{Name: f[0], Hash: f[1], Date: f[3], Subject: f[4]}
		if f[2] != "" {
			t.Hash = f[2] // annotated tag: point at the tagged commit
		}
		tags = append(tags, t)
	}
	return tags, nil
}

func (r Repo) loadHead() (Head, error) {
	var h Head
	top, err := r.run("rev-parse", "--show-toplevel")
	if err != nil {
		return h, err
	}
	h.Toplevel = strings.TrimSpace(top)
	if out, err := r.run("rev-parse", "HEAD"); err == nil {
		h.Hash = strings.TrimSpace(out)
	}
	if out, err := r.run("symbolic-ref", "--short", "-q", "HEAD"); err == nil {
		h.Branch = strings.TrimSpace(out)
	}
	if out, err := r.run("rev-parse", "--absolute-git-dir"); err == nil {
		gd := strings.TrimSpace(out)
		switch {
		case exists(filepath.Join(gd, "rebase-merge")), exists(filepath.Join(gd, "rebase-apply")):
			h.State = "REBASING"
		case exists(filepath.Join(gd, "MERGE_HEAD")):
			h.State = "MERGING"
		case exists(filepath.Join(gd, "CHERRY_PICK_HEAD")):
			h.State = "CHERRY-PICKING"
		case exists(filepath.Join(gd, "REVERT_HEAD")):
			h.State = "REVERTING"
		}
	}
	if files, err := r.loadStatus(); err == nil {
		h.Files, h.Dirty = files, len(files)
	}
	if out, err := r.run("remote"); err == nil {
		h.Remotes = strings.Fields(out)
	}
	return h, nil
}

// loadStatus returns path -> porcelain XY for every changed or untracked file.
func (r Repo) loadStatus() (map[string]string, error) {
	out, err := r.run("status", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	files := map[string]string{}
	ents := strings.Split(out, "\x00")
	for i := 0; i < len(ents); i++ {
		e := ents[i]
		if len(e) < 4 {
			continue
		}
		files[e[3:]] = e[:2]
		if e[0] == 'R' || e[0] == 'C' {
			i++ // -z puts the rename source in its own field
		}
	}
	return files, nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func (r Repo) loadBranches() (locals, remotes []Branch, err error) {
	fields := []string{
		"%(refname)", "%(refname:short)", "%(objectname)", "%(upstream:short)",
		"%(upstream:track,nobracket)", "%(worktreepath)", "%(committerdate:relative)", "%(contents:subject)",
	}
	out, err := r.run("for-each-ref", "--sort=-committerdate", "--format="+strings.Join(fields, "%1f"), "refs/heads", "refs/remotes")
	if err != nil {
		return nil, nil, err
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\x1f")
		if len(f) < len(fields) || strings.HasSuffix(f[0], "/HEAD") {
			continue
		}
		b := Branch{Name: f[1], Hash: f[2], Upstream: f[3], Track: f[4], Worktree: f[5], Date: f[6], Subject: f[7]}
		if strings.HasPrefix(f[0], "refs/remotes/") {
			b.Remote = true
			remotes = append(remotes, b)
		} else {
			locals = append(locals, b)
		}
	}
	return locals, remotes, nil
}

func (r Repo) loadWorktrees() ([]Worktree, error) {
	out, err := r.run("worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var wts []Worktree
	var cur *Worktree
	for _, line := range strings.Split(out, "\n") {
		key, val, _ := strings.Cut(line, " ")
		switch key {
		case "worktree":
			wts = append(wts, Worktree{Path: val})
			cur = &wts[len(wts)-1]
		case "HEAD":
			cur.Hash = val
		case "branch":
			cur.Branch = strings.TrimPrefix(val, "refs/heads/")
		case "detached":
			cur.Detached = true
		case "bare":
			cur.Bare = true
		case "locked":
			cur.Locked = true
		case "prunable":
			cur.Prunable = true
		}
	}
	return wts, nil
}

func (r Repo) loadGraph(max int, locals map[string]bool, head Head) ([]Row, error) {
	// --all already covers HEADs of every worktree; stash is excluded because its
	// internal index/untracked commits clutter the graph.
	args := []string{
		"log", "--date-order", "--color=never", "--exclude=refs/stash", "--all",
		"--date=format:%Y-%m-%d %H:%M",
		"--format=%H%x1f%P%x1f%h%x1f%an%x1f%ad%x1f%D%x1f%s",
	}
	if max > 0 {
		args = append(args, fmt.Sprintf("--max-count=%d", max))
	}
	out, err := r.run(args...)
	if err != nil {
		return nil, err
	}
	var commits []Row
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		f := strings.SplitN(line, "\x1f", 7)
		if len(f) < 7 {
			continue
		}
		commits = append(commits, Row{
			Hash: f[0], Parents: strings.Fields(f[1]), Short: f[2], Author: f[3], Date: f[4],
			Refs: parseRefs(f[5], locals), Subject: f[6],
		})
	}
	return layoutGraph(commits, head.Hash, head.Dirty), nil
}

func parseRefs(d string, locals map[string]bool) []Ref {
	var refs []Ref
	for _, p := range strings.Split(d, ", ") {
		switch {
		case p == "", strings.HasSuffix(p, "/HEAD"):
		case p == "HEAD":
			refs = append(refs, Ref{"HEAD", RefHead})
		case strings.HasPrefix(p, "HEAD -> "):
			refs = append(refs, Ref{"HEAD", RefHead}, Ref{p[len("HEAD -> "):], RefLocal})
		case strings.HasPrefix(p, "tag: "):
			refs = append(refs, Ref{p[len("tag: "):], RefTag})
		case locals[p]:
			refs = append(refs, Ref{p, RefLocal})
		default:
			refs = append(refs, Ref{p, RefRemote})
		}
	}
	return refs
}

// splitArgs splits a command line on whitespace, honoring single/double quotes.
func splitArgs(s string) []string {
	var args []string
	var cur strings.Builder
	var quote rune
	inArg := false
	for _, c := range s {
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				cur.WriteRune(c)
			}
		case c == '\'' || c == '"':
			quote, inArg = c, true
		case c == ' ' || c == '\t':
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(c)
			inArg = true
		}
	}
	if inArg {
		args = append(args, cur.String())
	}
	return args
}
