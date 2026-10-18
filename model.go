package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type panel int

const (
	pWorktrees panel = iota
	pBranches
	pGraph
	pDetail
	pDiff
)

type brTab int

const (
	tabLocal brTab = iota
	tabRemote
	tabTags
	brTabCount
)

var brTabNames = []string{"Local", "Remotes", "Tags"}

type uiMode int

const (
	modeNormal uiMode = iota
	modeConfirm
	modeInput
	modeMenu
	modeError
	modeHelp
	modeSearch
)

type mark struct{ ref, label, hash string }

type confirmState struct {
	title  string
	prompt string
	run    func() tea.Cmd
}

type menuItem struct {
	key   string
	label string
	run   func() tea.Cmd
}

type menuState struct {
	title string
	items []menuItem
	cur   int
}

type opOpts struct {
	jumpHead  bool
	switchTo  string
	onErr     func(out string) tea.Cmd
	onSuccess func()
	then      func() tea.Cmd // next operation to start after success
	// statusOnly skips the full reload: staging changes neither refs nor the
	// HEAD→worktree diff, only each file's index state.
	statusOnly bool
}

type statusMsg struct {
	seq   int
	files map[string]string
}

type snapshotMsg struct {
	seq  int
	snap *Snapshot
	err  error
}

type opDoneMsg struct {
	title string
	out   string
	err   error
}

type Model struct {
	repo     Repo
	maxCount int

	snap       *Snapshot
	hashRow    map[string]int
	worktreeAt map[string][]Worktree
	loadSeq    int
	jumpHead   bool

	width, height int
	focus         panel
	listFocus     panel // last focused list panel; drives the diff panel while it has focus
	brTab         brTab
	brCur, brOff  [brTabCount]int
	brView        [brTabCount][]int // indices into Locals/Remotes/Tags that pass brFilter
	brFilter      string
	wtCur, wtOff  int
	gCur, gOff    int

	mark   *mark
	cherry []string

	mode     uiMode
	confirm  confirmState
	menu     menuState
	input    textinput.Model
	inTitle  string
	inSubmit func(string) tea.Cmd
	errText  string
	search   string
	// searchPanel is where "/" was pressed; branches filter, other panels jump.
	searchPanel  panel
	searchOrigin int
	detailOff    int
	detailSel    string // file selected in the commit pane's tree
	detailAnchor string // other end of a range selection ("" = none)
	detailKey    string // diff key the selection belongs to

	diffOpen     bool
	zoomed       bool // focused pane fills the screen
	dv           diffPanel
	diffCache    map[string][]sbsRow
	diffSeq      int
	diffCtx      int
	diffIgnoreWS bool
	diffUnified  bool
	diffNoWrap   bool
	threeDot     bool

	mouseJunkUntil time.Time
	hl             highlight

	pending   opOpts
	status    string
	statusErr bool
	busy      string
}

func newModel(dir string, maxCount int) *Model {
	ti := textinput.New()
	ti.PromptStyle = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	ti.Prompt = ""
	return &Model{
		repo: Repo{Dir: dir}, maxCount: maxCount, input: ti,
		focus: pGraph, listFocus: pGraph, diffCtx: 3, diffOpen: true, diffCache: map[string][]sbsRow{},
	}
}

func (m *Model) Init() tea.Cmd { return m.reload() }

func (m *Model) reload() tea.Cmd {
	m.loadSeq++
	seq, repo, max := m.loadSeq, m.repo, m.maxCount
	return func() tea.Msg {
		s, err := repo.Load(max)
		return snapshotMsg{seq: seq, snap: s, err: err}
	}
}

func (m *Model) reloadStatus() tea.Cmd {
	seq, repo := m.loadSeq, m.repo
	return func() tea.Msg {
		files, _ := repo.loadStatus()
		return statusMsg{seq: seq, files: files}
	}
}

func (m *Model) setStatus(s string) { m.status, m.statusErr = s, false }

func (m *Model) showError(s string) {
	m.errText = strings.TrimSpace(s)
	m.mode = modeError
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "hint:") {
			return l
		}
	}
	return s
}

func lastLine(s string) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(ls) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(ls[i]); l != "" && !strings.HasPrefix(l, "hint:") {
			return l
		}
	}
	return ""
}

func samePath(a, b string) bool {
	ea, err1 := filepath.EvalSymlinks(a)
	eb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ea == eb
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func clamp(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	return min(max(v, lo), hi)
}

func (m *Model) applySnapshot(s *Snapshot) {
	prevHash, prevVirtual, prevPos := "", false, 0
	if m.snap != nil && m.gCur < len(m.snap.Rows) {
		r := m.snap.Rows[m.gCur]
		prevHash, prevVirtual, prevPos = r.Hash, r.Virtual, m.gCur-m.gOff
	}
	first := m.snap == nil

	m.snap = s
	m.hashRow = map[string]int{}
	for i, r := range s.Rows {
		if r.IsCommit() {
			m.hashRow[r.Hash] = i
		}
	}
	m.worktreeAt = map[string][]Worktree{}
	for _, wt := range s.Worktrees {
		if wt.Hash != "" && !samePath(wt.Path, s.Head.Toplevel) {
			m.worktreeAt[wt.Hash] = append(m.worktreeAt[wt.Hash], wt)
		}
	}
	m.recomputeBrView()
	for t := brTab(0); t < brTabCount; t++ {
		m.brCur[t] = clamp(m.brCur[t], 0, m.brLen(t)-1)
	}
	m.wtCur = clamp(m.wtCur, 0, len(s.Worktrees)-1)
	m.ensureListsVisible()
	for k := range m.diffCache {
		if !strings.HasPrefix(k, "show:") {
			delete(m.diffCache, k)
		}
	}
	m.dv.key, m.dv.pending = "", ""
	m.hl = highlight{}
	m.followList()

	switch {
	case first || m.jumpHead:
		m.jumpHead = false
		m.gCur = 0
		if i, ok := m.hashRow[s.Head.Hash]; ok {
			m.gCur = i
		}
		m.ensureVisible()
	case prevVirtual:
		if i, ok := m.hashRow[s.Head.Hash]; ok {
			m.gCur = i
			if i > 0 && s.Rows[i-1].Virtual {
				m.gCur = i - 1
			}
		}
		m.ensureVisible()
	case prevHash != "":
		// Keep the selected commit at the same screen row so the view does not
		// jump when commits appear above it.
		if i, ok := m.hashRow[prevHash]; ok {
			m.gCur = i
			m.gOff = max(0, i-prevPos)
		}
		m.ensureVisible()
	default:
		m.ensureVisible()
	}
}

// ---- cursor & scrolling ----

func (m *Model) ensureVisible() {
	n := len(m.snap.Rows)
	h := m.graphInnerHeight()
	m.gCur = clamp(m.gCur, 0, n-1)
	if h <= 0 {
		return
	}
	margin := min(3, h/4)
	if m.gCur < m.gOff+margin {
		m.gOff = m.gCur - margin
	}
	if m.gCur > m.gOff+h-1-margin {
		m.gOff = m.gCur - h + 1 + margin
	}
	m.gOff = clamp(m.gOff, 0, n-h)
}

func ensureListVisible(cur, off *int, n, h int) {
	if h <= 0 {
		return
	}
	if *cur < *off {
		*off = *cur
	}
	if *cur >= *off+h {
		*off = *cur - h + 1
	}
	*off = clamp(*off, 0, n-h)
}

func (m *Model) ensureListsVisible() {
	if m.snap == nil {
		return
	}
	l := m.layout()
	ensureListVisible(&m.wtCur, &m.wtOff, len(m.snap.Worktrees), l.wtH-2)
	for t := brTab(0); t < brTabCount; t++ {
		ensureListVisible(&m.brCur[t], &m.brOff[t], m.brLen(t), l.brH-2)
	}
	m.ensureVisible()
}

func (m *Model) moveGraph(n int) {
	rows := m.snap.Rows
	i, step := m.gCur, 1
	if n < 0 {
		step, n = -1, -n
	}
	for ; n > 0; n-- {
		j := i + step
		for j >= 0 && j < len(rows) && !rows[j].Selectable() {
			j += step
		}
		if j < 0 || j >= len(rows) {
			break
		}
		i = j
	}
	m.gCur = i
	m.ensureVisible()
}

func (m *Model) brLen(t brTab) int { return len(m.brView[t]) }

func (m *Model) brName(t brTab, idx int) string {
	switch t {
	case tabLocal:
		return m.snap.Locals[idx].Name
	case tabRemote:
		return m.snap.Remotes[idx].Name
	default:
		return m.snap.Tags[idx].Name
	}
}

func (m *Model) recomputeBrView() {
	q := strings.ToLower(m.brFilter)
	counts := [brTabCount]int{len(m.snap.Locals), len(m.snap.Remotes), len(m.snap.Tags)}
	for t := brTab(0); t < brTabCount; t++ {
		view := make([]int, 0, counts[t])
		for i := 0; i < counts[t]; i++ {
			if q == "" || strings.Contains(strings.ToLower(m.brName(t, i)), q) {
				view = append(view, i)
			}
		}
		m.brView[t] = view
	}
}

func (m *Model) setBrFilter(q string) {
	m.brFilter = q
	m.recomputeBrView()
	for t := brTab(0); t < brTabCount; t++ {
		m.brCur[t], m.brOff[t] = 0, 0
	}
	m.followList()
}

func (m *Model) moveList(p panel, n int) {
	l := m.layout()
	switch p {
	case pGraph:
		m.moveGraph(n)
	case pBranches:
		t := m.brTab
		m.brCur[t] = clamp(m.brCur[t]+n, 0, m.brLen(t)-1)
		ensureListVisible(&m.brCur[t], &m.brOff[t], m.brLen(t), l.brH-2)
		m.followList()
	case pWorktrees:
		m.wtCur = clamp(m.wtCur+n, 0, len(m.snap.Worktrees)-1)
		ensureListVisible(&m.wtCur, &m.wtOff, len(m.snap.Worktrees), l.wtH-2)
		m.followList()
	case pDiff:
		m.scrollDiff(n)
	case pDetail:
		m.moveDetailFile(n)
	}
}

func (m *Model) pageSize(p panel) int {
	l := m.layout()
	switch p {
	case pBranches:
		return max(l.brH-2, 1)
	case pWorktrees:
		return max(l.wtH-2, 1)
	case pDiff:
		return max(m.diffInnerHeight(), 1)
	case pDetail:
		return max(l.detailH-2, 1)
	}
	return max(m.graphInnerHeight()-1, 1)
}

// followList moves the graph cursor to the selected ref's commit without
// reordering the graph, so the ref's position in the whole tree stays visible.
func (m *Model) followList() {
	if m.focus != pBranches && m.focus != pWorktrees {
		return
	}
	t, ok := m.listTarget(m.focus)
	if !ok {
		return
	}
	m.updateHighlight(t)
	if i, ok := m.hashRow[t.hash]; ok {
		m.gCur = i
		m.ensureVisible()
	}
}

// highlight marks the ref selected in a side panel and the commits it has that
// HEAD does not, so the branch's extent is visible in the full graph.
type highlight struct {
	key   string
	label string
	hash  string
	only  map[string]bool
}

func (m *Model) highlightActive() bool {
	return m.focus == pBranches || m.focus == pWorktrees
}

func (m *Model) updateHighlight(t target) {
	key := t.ref + "@" + t.hash + "^" + m.snap.Head.Hash
	if key == m.hl.key {
		return
	}
	m.hl = highlight{key: key, label: t.label, hash: t.hash}
	if t.hash == "" || t.hash == m.snap.Head.Hash {
		return
	}
	out, err := m.quick("rev-list", "--max-count=2000", "HEAD.."+t.hash)
	if err != nil || out == "" {
		return
	}
	m.hl.only = map[string]bool{}
	for _, h := range strings.Split(out, "\n") {
		m.hl.only[h] = true
	}
}

func (m *Model) jumpToHead() {
	if i, ok := m.hashRow[m.snap.Head.Hash]; ok {
		m.gCur = i
		m.ensureVisible()
	}
}

func (m *Model) setFocus(p panel) {
	if (p == pDiff || p == pDetail) && !m.diffOpen {
		m.diffOpen = true
	}
	m.focus = p
	if p != pDiff && p != pDetail {
		m.listFocus = p
	}
	m.followList()
}

func (m *Model) cycleFocus(d int) {
	order := []panel{pWorktrees, pBranches, pGraph}
	if m.diffOpen {
		order = append(order, pDetail, pDiff)
	}
	i := 0
	for j, p := range order {
		if p == m.focus {
			i = j
		}
	}
	m.setFocus(order[(i+d+len(order))%len(order)])
}

// ---- targets ----

type targetKind int

const (
	tkCommit targetKind = iota
	tkVirtual
	tkLocal
	tkRemote
	tkTag
	tkWorktree
)

type target struct {
	kind   targetKind
	ref    string
	label  string
	hash   string
	local  *Branch
	remote *Branch
	tag    *Tag
	wt     *Worktree
	row    *Row
}

func (m *Model) listTarget(p panel) (target, bool) {
	if m.snap == nil {
		return target{}, false
	}
	switch p {
	case pBranches:
		view := m.brView[m.brTab]
		if m.brCur[m.brTab] >= len(view) {
			return target{}, false
		}
		c := view[m.brCur[m.brTab]]
		switch m.brTab {
		case tabLocal:
			if c < len(m.snap.Locals) {
				b := &m.snap.Locals[c]
				return target{kind: tkLocal, ref: b.Name, label: b.Name, hash: b.Hash, local: b}, true
			}
		case tabRemote:
			if c < len(m.snap.Remotes) {
				b := &m.snap.Remotes[c]
				return target{kind: tkRemote, ref: b.Name, label: b.Name, hash: b.Hash, remote: b}, true
			}
		case tabTags:
			if c < len(m.snap.Tags) {
				t := &m.snap.Tags[c]
				return target{kind: tkTag, ref: "refs/tags/" + t.Name, label: t.Name, hash: t.Hash, tag: t}, true
			}
		}
	case pWorktrees:
		if m.wtCur < len(m.snap.Worktrees) {
			w := &m.snap.Worktrees[m.wtCur]
			ref := w.Branch
			if ref == "" {
				ref = w.Hash
			}
			return target{kind: tkWorktree, ref: ref, label: worktreeLabel(*w), hash: w.Hash, wt: w}, true
		}
	case pGraph:
		if m.gCur < len(m.snap.Rows) {
			row := &m.snap.Rows[m.gCur]
			if row.Virtual {
				return target{kind: tkVirtual, ref: "", label: "working tree", row: row}, true
			}
			if row.IsCommit() {
				return target{kind: tkCommit, ref: row.Hash, label: row.Short, hash: row.Hash, row: row}, true
			}
		}
	case pDiff, pDetail:
		return m.listTarget(m.listFocus)
	}
	return target{}, false
}

func (m *Model) findLocal(name string) *Branch {
	for i := range m.snap.Locals {
		if m.snap.Locals[i].Name == name {
			return &m.snap.Locals[i]
		}
	}
	return nil
}

func (m *Model) headLabel() string {
	if m.snap.Head.Branch != "" {
		return m.snap.Head.Branch
	}
	if len(m.snap.Head.Hash) >= 7 {
		return m.snap.Head.Hash[:7]
	}
	return "HEAD"
}

func (m *Model) defaultRemote() string {
	rs := m.snap.Head.Remotes
	for _, r := range rs {
		if r == "origin" {
			return r
		}
	}
	if len(rs) > 0 {
		return rs[0]
	}
	return ""
}

// ---- popups ----

func (m *Model) ask(title, prompt string, run func() tea.Cmd) {
	m.mode = modeConfirm
	m.confirm = confirmState{title: title, prompt: prompt, run: run}
}

func (m *Model) prompt(title, value string, submit func(string) tea.Cmd) tea.Cmd {
	m.mode = modeInput
	m.inTitle = title
	m.inSubmit = submit
	m.input.SetValue(value)
	m.input.CursorEnd()
	return m.input.Focus()
}

func (m *Model) openMenu(title string, items []menuItem) {
	m.mode = modeMenu
	m.menu = menuState{title: title, items: items}
}

// ---- running git ----

func (m *Model) gitOp(title string, opts opOpts, args ...string) tea.Cmd {
	return m.gitOpEnv(title, opts, nil, args...)
}

func (m *Model) gitOpEnv(title string, opts opOpts, env []string, args ...string) tea.Cmd {
	if m.busy != "" {
		m.setStatus("busy: " + m.busy)
		return nil
	}
	m.busy, m.pending, m.status = title, opts, ""
	repo := m.repo
	return func() tea.Msg {
		out, err := repo.runEnv(env, args...)
		return opDoneMsg{title: title, out: out, err: err}
	}
}

func (m *Model) execOp(title string, opts opOpts, env []string, args ...string) tea.Cmd {
	if m.busy != "" {
		m.setStatus("busy: " + m.busy)
		return nil
	}
	m.pending = opts
	cmd := exec.Command("git", args...)
	cmd.Dir = m.repo.Dir
	cmd.Env = append(os.Environ(), env...)
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return opDoneMsg{title: title, err: err}
	})
}

// execShell runs a user-typed shell command and waits for Enter so its output
// can be read before the TUI redraws.
func (m *Model) execShell(command string) tea.Cmd {
	sh := os.Getenv("SHELL")
	if sh == "" {
		sh = "sh"
	}
	script := command + `; s=$?; printf '\n[exit %d] press Enter to return ' $s; read _`
	cmd := exec.Command(sh, "-c", script)
	cmd.Dir = m.repo.Dir
	m.pending = opOpts{}
	return tea.ExecProcess(cmd, func(error) tea.Msg {
		return opDoneMsg{title: command}
	})
}

func (m *Model) quick(args ...string) (string, error) {
	out, err := m.repo.run(args...)
	return strings.TrimSpace(out), err
}

// ---- operations ----

func (m *Model) switchWorktree(path string) tea.Cmd {
	if _, err := os.Stat(path); err != nil {
		m.showError("worktree missing: " + path)
		return nil
	}
	m.repo.Dir = path
	m.jumpHead = true
	m.setStatus("switched to worktree " + path)
	return m.reload()
}

func (m *Model) checkoutLocal(name string) tea.Cmd {
	if name == m.snap.Head.Branch {
		m.setStatus("already on " + name)
		return nil
	}
	if b := m.findLocal(name); b != nil && b.Worktree != "" && !samePath(b.Worktree, m.snap.Head.Toplevel) {
		cmd := m.switchWorktree(b.Worktree)
		m.setStatus(name + " is checked out in another worktree → switched to " + b.Worktree)
		return cmd
	}
	return m.gitOp("checkout "+name, opOpts{jumpHead: true}, "switch", name)
}

func (m *Model) checkoutRemote(name string) tea.Cmd {
	_, local, _ := strings.Cut(name, "/")
	if m.findLocal(local) != nil {
		return m.checkoutLocal(local)
	}
	return m.gitOp("checkout "+name, opOpts{jumpHead: true}, "switch", "--track", name)
}

func (m *Model) checkout(t target) tea.Cmd {
	switch t.kind {
	case tkWorktree:
		if t.wt.Bare {
			m.showError("bare worktree")
			return nil
		}
		return m.switchWorktree(t.wt.Path)
	case tkLocal:
		return m.checkoutLocal(t.local.Name)
	case tkRemote:
		return m.checkoutRemote(t.remote.Name)
	case tkVirtual:
		return nil
	case tkTag:
		return m.detach(t)
	}
	var remote string
	for _, r := range t.row.Refs {
		if r.Kind == RefLocal {
			if r.Name == m.snap.Head.Branch {
				continue
			}
			return m.checkoutLocal(r.Name)
		}
		if r.Kind == RefRemote && remote == "" {
			remote = r.Name
		}
	}
	if remote != "" {
		return m.checkoutRemote(remote)
	}
	if t.hash == m.snap.Head.Hash {
		m.setStatus("HEAD is already here")
		return nil
	}
	return m.detach(t)
}

func (m *Model) detach(t target) tea.Cmd {
	m.ask("Checkout", fmt.Sprintf("Checkout %s as detached HEAD?", t.label), func() tea.Cmd {
		return m.gitOp("checkout "+t.label, opOpts{jumpHead: true}, "switch", "--detach", t.hash)
	})
	return nil
}

func (m *Model) push() tea.Cmd {
	h := m.snap.Head
	if h.Branch == "" {
		m.showError("cannot push: detached HEAD")
		return nil
	}
	args := []string{"push"}
	if h.Upstream == "" {
		remote := m.defaultRemote()
		if remote == "" {
			m.showError("no remote configured")
			return nil
		}
		args = append(args, "--set-upstream", remote, h.Branch)
	}
	return m.gitOp("push", opOpts{onErr: func(out string) tea.Cmd {
		if strings.Contains(out, "rejected") || strings.Contains(out, "non-fast-forward") {
			m.ask("Force push", "Updates were rejected. Force push with --force-with-lease?", func() tea.Cmd {
				return m.gitOp("force push", opOpts{}, append(args, "--force-with-lease")...)
			})
		}
		return nil
	}}, args...)
}

func (m *Model) fastForward(b *Branch) tea.Cmd {
	if b.Upstream == "" {
		m.showError(b.Name + " has no upstream")
		return nil
	}
	if b.Name == m.snap.Head.Branch {
		return m.gitOp("fast-forward "+b.Name, opOpts{}, "merge", "--ff-only", "@{u}")
	}
	remote, rb, _ := strings.Cut(b.Upstream, "/")
	return m.gitOp("fast-forward "+b.Name, opOpts{}, "fetch", remote, rb+":"+b.Name)
}

func (m *Model) deleteLocal(name string) tea.Cmd {
	return m.gitOp("delete "+name, opOpts{onErr: func(out string) tea.Cmd {
		if strings.Contains(out, "not fully merged") {
			m.ask("Force delete", name+" is not fully merged. Delete anyway (-D)?", func() tea.Cmd {
				return m.gitOp("delete "+name, opOpts{}, "branch", "-D", name)
			})
		}
		return nil
	}}, "branch", "-d", name)
}

func (m *Model) deleteMenu(t target) tea.Cmd {
	switch t.kind {
	case tkLocal:
		b := t.local
		if b.Name == m.snap.Head.Branch {
			m.showError("cannot delete the checked-out branch")
			return nil
		}
		items := []menuItem{{"c", "Delete local branch " + b.Name, func() tea.Cmd { return m.deleteLocal(b.Name) }}}
		if b.Upstream != "" {
			remote, rb, _ := strings.Cut(b.Upstream, "/")
			items = append(items,
				menuItem{"r", "Delete remote branch " + b.Upstream, func() tea.Cmd {
					return m.gitOp("delete "+b.Upstream, opOpts{}, "push", remote, "--delete", rb)
				}},
				menuItem{"b", "Delete local and remote branch", func() tea.Cmd {
					return m.gitOp("delete "+b.Upstream, opOpts{then: func() tea.Cmd { return m.deleteLocal(b.Name) }},
						"push", remote, "--delete", rb)
				}})
		}
		m.openMenu("Delete branch", items)
	case tkRemote:
		remote, rb, _ := strings.Cut(t.remote.Name, "/")
		m.ask("Delete remote branch", fmt.Sprintf("Delete %s from %s?", rb, remote), func() tea.Cmd {
			return m.gitOp("delete "+t.remote.Name, opOpts{}, "push", remote, "--delete", rb)
		})
	case tkTag:
		name := t.tag.Name
		items := []menuItem{{"c", "Delete local tag " + name, func() tea.Cmd {
			return m.gitOp("delete tag "+name, opOpts{}, "tag", "-d", name)
		}}}
		if r := m.defaultRemote(); r != "" {
			items = append(items, menuItem{"r", "Delete remote tag on " + r, func() tea.Cmd {
				return m.gitOp("delete remote tag "+name, opOpts{}, "push", r, "--delete", "refs/tags/"+name)
			}})
		}
		m.openMenu("Delete tag", items)
	case tkWorktree:
		wt := *t.wt
		if len(m.snap.Worktrees) > 0 && samePath(wt.Path, m.snap.Worktrees[0].Path) {
			m.showError("cannot remove the main worktree")
			return nil
		}
		m.ask("Remove worktree", "Remove worktree "+wt.Path+"?", func() tea.Cmd {
			if samePath(wt.Path, m.snap.Head.Toplevel) {
				m.repo.Dir = m.snap.Worktrees[0].Path
			}
			return m.gitOp("remove worktree", opOpts{jumpHead: true, onErr: func(out string) tea.Cmd {
				if strings.Contains(out, "--force") {
					m.ask("Force remove", "Worktree has changes. Remove anyway?", func() tea.Cmd {
						return m.gitOp("remove worktree", opOpts{}, "worktree", "remove", "--force", wt.Path)
					})
				}
				return nil
			}}, "worktree", "remove", wt.Path)
		})
	case tkCommit:
		return m.rewrite(t, "drop")
	}
	return nil
}

func (m *Model) newWorktree(t target) tea.Cmd {
	suggest := ""
	if t.kind == tkLocal {
		suggest = t.local.Name
	}
	from := t.ref
	if t.kind == tkVirtual {
		from = "HEAD"
	}
	return m.prompt("New worktree from "+t.label+" (branch name)", suggest, func(branch string) tea.Cmd {
		main := m.snap.Head.Toplevel
		if len(m.snap.Worktrees) > 0 {
			main = m.snap.Worktrees[0].Path
		}
		dir := filepath.Join(filepath.Dir(main), filepath.Base(main)+"-"+strings.ReplaceAll(branch, "/", "-"))
		args := []string{"worktree", "add", "-b", branch, dir, from}
		if m.findLocal(branch) != nil {
			args = []string{"worktree", "add", dir, branch}
		}
		return m.gitOp("new worktree "+dir, opOpts{switchTo: dir}, args...)
	})
}

func (m *Model) newBranch(t target) tea.Cmd {
	from := t.ref
	if t.kind == tkVirtual {
		from = "HEAD"
	}
	suggest := ""
	if t.kind == tkRemote {
		_, suggest, _ = strings.Cut(t.remote.Name, "/")
	}
	return m.prompt("New branch name (from "+t.label+")", suggest, func(name string) tea.Cmd {
		return m.gitOp("new branch "+name, opOpts{jumpHead: true}, "switch", "-c", name, from)
	})
}

func (m *Model) discardMenu() {
	clean := func() tea.Cmd { return m.gitOp("clean", opOpts{}, "clean", "-fd") }
	m.openMenu("Discard uncommitted changes", []menuItem{
		{"a", "Discard all changes (including untracked files)", func() tea.Cmd {
			m.ask("Discard", "Discard ALL uncommitted changes and delete untracked files?", func() tea.Cmd {
				return m.gitOp("discard", opOpts{then: clean}, "reset", "--hard", "HEAD")
			})
			return nil
		}},
		{"t", "Discard changes to tracked files (keep untracked)", func() tea.Cmd {
			m.ask("Discard", "Discard all changes to tracked files?", func() tea.Cmd {
				return m.gitOp("discard", opOpts{}, "reset", "--hard", "HEAD")
			})
			return nil
		}},
		{"u", "Delete untracked files only", func() tea.Cmd {
			m.ask("Discard", "Delete all untracked files?", clean)
			return nil
		}},
	})
}

// showingWorktree reports whether the commit pane lists uncommitted changes
// (not a commit or a diff-mode comparison), where file actions apply.
func (m *Model) showingWorktree() bool {
	t, ok := m.listTarget(m.listFocus)
	return ok && t.kind == tkVirtual && m.mark == nil
}

func (m *Model) toggleRangeSelect() {
	switch {
	case m.detailSel == "":
	case m.detailAnchor == "":
		m.detailAnchor = m.detailSel
		m.setStatus("range select: j/k to extend, v/esc to cancel")
	default:
		m.detailAnchor = ""
	}
}

func filesLabel(paths []string) string {
	if len(paths) == 1 {
		return paths[0]
	}
	return fmt.Sprintf("%d files", len(paths))
}

func (m *Model) toggleStageFile() tea.Cmd {
	paths := m.selectedFiles()
	if !m.showingWorktree() || len(paths) == 0 {
		return nil
	}
	allStaged := true
	for _, p := range paths {
		xy := m.snap.Head.Files[p]
		allStaged = allStaged && len(xy) == 2 && xy[0] != ' ' && xy[0] != '?' && xy[1] == ' '
	}
	if allStaged {
		return m.gitOp("unstage "+filesLabel(paths), opOpts{statusOnly: true}, append([]string{"restore", "--staged", "--"}, paths...)...)
	}
	return m.gitOp("stage "+filesLabel(paths), opOpts{statusOnly: true}, append([]string{"add", "-A", "--"}, paths...)...)
}

func (m *Model) toggleStageAll() tea.Cmd {
	if !m.showingWorktree() {
		return nil
	}
	for _, xy := range m.snap.Head.Files {
		if xy[1] != ' ' {
			return m.gitOp("stage all", opOpts{statusOnly: true}, "add", "-A")
		}
	}
	return m.gitOp("unstage all", opOpts{statusOnly: true}, "reset", "-q")
}

func (m *Model) commitStaged() tea.Cmd {
	if !m.showingWorktree() {
		return nil
	}
	staged := false
	for _, xy := range m.snap.Head.Files {
		staged = staged || (xy[0] != ' ' && xy[0] != '?')
	}
	if !staged {
		m.showError("Nothing staged. Stage files with space (or a for all) first.")
		return nil
	}
	return m.prompt("Commit message", "", func(msg string) tea.Cmd {
		return m.gitOp("commit", opOpts{jumpHead: true}, "commit", "-m", msg)
	})
}

// discardFile discards the selected files while the commit pane shows the
// working tree; untracked files are deleted.
func (m *Model) discardFile() tea.Cmd {
	paths := m.selectedFiles()
	if !m.showingWorktree() || len(paths) == 0 {
		return nil
	}
	var tracked, untracked []string
	for _, p := range paths {
		if m.snap.Head.Files[p] == "??" {
			untracked = append(untracked, p)
		} else {
			tracked = append(tracked, p)
		}
	}
	prompt := fmt.Sprintf("Discard changes to %s?", filesLabel(paths))
	if len(paths) > 1 {
		prompt += "\n\n" + strings.Join(paths, "\n")
	}
	m.ask("Discard", prompt, func() tea.Cmd {
		m.detailAnchor = ""
		label := "discard " + filesLabel(paths)
		clean := func() tea.Cmd {
			return m.gitOp(label, opOpts{}, append([]string{"clean", "-f", "--"}, untracked...)...)
		}
		if len(tracked) == 0 {
			return clean()
		}
		var opts opOpts
		if len(untracked) > 0 {
			opts.then = clean
		}
		// Restoring from HEAD also removes files that were added to the index.
		return m.gitOp(label, opts, append([]string{"restore", "--source=HEAD", "--staged", "--worktree", "--"}, tracked...)...)
	})
	return nil
}

func (m *Model) resetMenu(t target) tea.Cmd {
	ref := t.ref
	if t.kind == tkTag {
		ref = t.hash
	}
	item := func(k, mode string) menuItem {
		return menuItem{k, fmt.Sprintf("%s reset to %s", mode, t.label), func() tea.Cmd {
			run := func() tea.Cmd {
				return m.gitOp(mode+" reset", opOpts{jumpHead: true}, "reset", "--"+mode, ref)
			}
			if mode == "hard" {
				m.ask("Hard reset", "Discard all changes and reset "+m.headLabel()+" to "+t.label+"?", run)
				return nil
			}
			return run()
		}}
	}
	m.openMenu("Reset "+m.headLabel()+" to "+t.label, []menuItem{item("m", "mixed"), item("s", "soft"), item("h", "hard")})
	return nil
}

func (m *Model) mergeRebaseMenu() tea.Cmd {
	var verb string
	switch m.snap.Head.State {
	case "REBASING":
		verb = "rebase"
	case "MERGING":
		verb = "merge"
	case "CHERRY-PICKING":
		verb = "cherry-pick"
	case "REVERTING":
		verb = "revert"
	default:
		m.setStatus("no merge/rebase in progress")
		return nil
	}
	items := []menuItem{
		{"c", "continue", func() tea.Cmd {
			return m.execOp(verb+" --continue", opOpts{jumpHead: true}, nil, verb, "--continue")
		}},
		{"a", "abort", func() tea.Cmd {
			return m.gitOp(verb+" --abort", opOpts{jumpHead: true}, verb, "--abort")
		}},
	}
	if verb != "merge" {
		items = append(items, menuItem{"s", "skip", func() tea.Cmd {
			return m.gitOp(verb+" --skip", opOpts{jumpHead: true}, verb, "--skip")
		}})
	}
	m.openMenu(capitalize(verb)+" options", items)
	return nil
}

func (m *Model) stashMenu() tea.Cmd {
	m.openMenu("Stash options", []menuItem{
		{"a", "stash all changes", func() tea.Cmd { return m.gitOp("stash", opOpts{}, "stash", "push") }},
		{"U", "stash all changes including untracked files", func() tea.Cmd {
			return m.gitOp("stash", opOpts{}, "stash", "push", "--include-untracked")
		}},
		{"s", "stash staged changes", func() tea.Cmd { return m.gitOp("stash", opOpts{}, "stash", "push", "--staged") }},
		{"g", "pop latest stash", func() tea.Cmd { return m.gitOp("stash pop", opOpts{}, "stash", "pop") }},
		{"space", "apply latest stash", func() tea.Cmd { return m.gitOp("stash apply", opOpts{}, "stash", "apply") }},
		{"d", "drop latest stash", func() tea.Cmd {
			m.ask("Drop stash", "Drop the latest stash entry?", func() tea.Cmd {
				return m.gitOp("stash drop", opOpts{}, "stash", "drop")
			})
			return nil
		}},
	})
	return nil
}

// rewrite edits history between the selected commit and HEAD via a scripted
// interactive rebase, like lazygit's commit actions (reword/drop/squash/fixup/edit).
func (m *Model) rewrite(t target, action string) tea.Cmd {
	if t.kind != tkCommit {
		return nil
	}
	if m.snap.Head.State != "" {
		m.showError("finish the current " + strings.ToLower(m.snap.Head.State) + " first (m)")
		return nil
	}
	if _, err := m.quick("merge-base", "--is-ancestor", t.hash, "HEAD"); err != nil {
		m.showError(t.label + " is not an ancestor of HEAD; only commits on the checked-out branch can be rewritten")
		return nil
	}
	line := 1
	target := t.hash
	if action == "squash" || action == "fixup" {
		parent, err := m.quick("rev-parse", "--verify", "-q", t.hash+"^")
		if err != nil {
			m.showError("cannot " + action + " the root commit")
			return nil
		}
		target, line = parent, 2
	}
	base := []string{"--root"}
	if p, err := m.quick("rev-parse", "--verify", "-q", target+"^"); err == nil {
		base = []string{p}
	}
	rangeSpec := "HEAD"
	if base[0] != "--root" {
		rangeSpec = base[0] + "..HEAD"
	}
	if merges, _ := m.quick("rev-list", "--merges", rangeSpec); merges != "" {
		m.showError("merge commits between " + t.label + " and HEAD; rewrite is not supported here")
		return nil
	}
	env := []string{fmt.Sprintf(`GIT_SEQUENCE_EDITOR=sed -i.bak -E '%ds/^(pick|p) /%s /'`, line, action)}
	args := append([]string{"rebase", "-i", "--autostash"}, base...)
	title := action + " " + t.label
	switch action {
	case "reword":
		if t.hash == m.snap.Head.Hash {
			return m.execOp(title, opOpts{}, nil, "commit", "--amend", "--only")
		}
		return m.execOp(title, opOpts{}, env, args...)
	case "drop":
		m.ask("Drop commit", fmt.Sprintf("Drop %s %q?", t.label, t.row.Subject), func() tea.Cmd {
			return m.gitOpEnv(title, opOpts{}, env, args...)
		})
		return nil
	case "squash", "fixup":
		m.ask(capitalize(action), fmt.Sprintf("%s %s into the commit below it?", capitalize(action), t.label), func() tea.Cmd {
			return m.gitOpEnv(title, opOpts{}, env, args...)
		})
		return nil
	}
	return m.gitOpEnv(title, opOpts{}, env, args...)
}

func (m *Model) toggleCherry(t target) {
	if t.kind != tkCommit {
		return
	}
	for i, h := range m.cherry {
		if h == t.hash {
			m.cherry = append(m.cherry[:i], m.cherry[i+1:]...)
			m.setStatus(fmt.Sprintf("%d commit(s) copied for cherry-pick", len(m.cherry)))
			return
		}
	}
	m.cherry = append(m.cherry, t.hash)
	m.setStatus(fmt.Sprintf("%d commit(s) copied for cherry-pick (V to paste)", len(m.cherry)))
}

func (m *Model) pasteCherry() tea.Cmd {
	if len(m.cherry) == 0 {
		m.setStatus("nothing copied (c to copy commits)")
		return nil
	}
	hs := append([]string{}, m.cherry...)
	// Graph rows are newest first; cherry-pick oldest first.
	sort.Slice(hs, func(i, j int) bool { return m.hashRow[hs[i]] > m.hashRow[hs[j]] })
	m.ask("Cherry-pick", fmt.Sprintf("Cherry-pick %d commit(s) onto %s?", len(hs), m.headLabel()), func() tea.Cmd {
		return m.gitOp("cherry-pick", opOpts{jumpHead: true, onSuccess: func() { m.cherry = nil }},
			append([]string{"cherry-pick"}, hs...)...)
	})
	return nil
}

func (m *Model) isCherry(hash string) bool {
	for _, h := range m.cherry {
		if h == hash {
			return true
		}
	}
	return false
}

func (m *Model) copyToClipboard(t target) {
	s := t.ref
	switch t.kind {
	case tkCommit:
		s = t.hash
	case tkTag:
		s = t.tag.Name
	case tkWorktree:
		s = t.wt.Path
	case tkVirtual:
		return
	}
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(s)
	if err := cmd.Run(); err != nil {
		m.showError("copy failed: " + err.Error())
		return
	}
	m.setStatus("copied " + s)
}

func (m *Model) toggleDiffMark(t target) {
	if m.mark != nil {
		m.mark = nil
		m.setStatus("exited diff mode")
		return
	}
	if t.kind == tkVirtual {
		return
	}
	ref := t.ref
	if t.kind == tkWorktree && t.wt.Branch == "" {
		ref = t.hash
	}
	m.mark = &mark{ref: ref, label: t.label, hash: t.hash}
	m.diffOpen = true
	m.setStatus("diffing from " + t.label + " — select another item; W or esc to exit")
}

// ---- search ----

func (m *Model) matches(p panel, i int, q string) bool {
	q = strings.ToLower(q)
	has := func(s ...string) bool {
		for _, x := range s {
			if strings.Contains(strings.ToLower(x), q) {
				return true
			}
		}
		return false
	}
	switch p {
	case pGraph:
		r := m.snap.Rows[i]
		if !r.IsCommit() {
			return false
		}
		if has(r.Hash, r.Subject, r.Author) {
			return true
		}
		for _, ref := range r.Refs {
			if has(ref.Name) {
				return true
			}
		}
	case pWorktrees:
		w := m.snap.Worktrees[i]
		return has(w.Path, w.Branch)
	case pBranches:
		switch m.brTab {
		case tabLocal:
			return has(m.snap.Locals[i].Name)
		case tabRemote:
			return has(m.snap.Remotes[i].Name)
		case tabTags:
			return has(m.snap.Tags[i].Name)
		}
	}
	return false
}

func (m *Model) searchNext(dir int) {
	if dir < 0 {
		m.searchDir(-1, 1)
		return
	}
	m.searchFrom(1)
}

func (m *Model) searchFrom(start int) { m.searchDir(1, start) }

func (m *Model) searchDir(dir, start int) {
	if m.search == "" {
		return
	}
	p := m.searchPanel
	var n, cur int
	switch p {
	case pGraph:
		n, cur = len(m.snap.Rows), m.gCur
	case pWorktrees:
		n, cur = len(m.snap.Worktrees), m.wtCur
	default:
		return
	}
	for k := start; k <= n; k++ {
		i := ((cur+dir*k)%n + n) % n
		if m.matches(p, i, m.search) {
			m.moveList(p, i-cur)
			if p == pGraph {
				m.gCur = i
				m.ensureVisible()
			}
			return
		}
	}
	m.setStatus("no match for " + m.search)
}

// ---- update ----

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.update(msg)
	return m, tea.Batch(cmd, m.syncDiff())
}

func (m *Model) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.ensureListsVisible()
		return nil

	case tea.FocusMsg:
		// Commits, fetches and edits made in another pane show up once the user
		// comes back.
		if m.busy == "" {
			return m.reload()
		}
		return nil

	case snapshotMsg:
		if msg.seq != m.loadSeq {
			return nil
		}
		if msg.err != nil {
			if m.snap == nil {
				m.errText = msg.err.Error()
				return nil
			}
			m.showError(msg.err.Error())
			return nil
		}
		m.applySnapshot(msg.snap)
		return nil

	case opDoneMsg:
		m.busy = ""
		opts := m.pending
		m.pending = opOpts{}
		var cmds []tea.Cmd
		if msg.err != nil {
			if opts.onErr != nil {
				cmds = append(cmds, opts.onErr(msg.err.Error()))
			}
			// onErr may have opened a follow-up prompt (e.g. force delete); only
			// show the raw error when it did not.
			if m.mode == modeNormal {
				m.showError(msg.title + " failed:\n\n" + msg.err.Error())
			}
		} else {
			s := msg.title + ": done"
			if l := lastLine(msg.out); l != "" {
				s = msg.title + ": " + l
			}
			m.setStatus(s)
			// e.g. "Applying autostash resulted in conflicts" still exits 0.
			if strings.Contains(strings.ToLower(msg.out), "conflict") {
				m.showError(msg.title + " finished with conflicts:\n\n" + msg.out)
			}
			if opts.switchTo != "" {
				m.repo.Dir = opts.switchTo
			}
			if opts.jumpHead || opts.switchTo != "" {
				m.jumpHead = true
			}
			if opts.onSuccess != nil {
				opts.onSuccess()
			}
			if opts.then != nil {
				cmds = append(cmds, opts.then())
			}
		}
		if opts.statusOnly && msg.err == nil {
			cmds = append(cmds, m.reloadStatus())
		} else {
			cmds = append(cmds, m.reload())
		}
		return tea.Batch(cmds...)

	case statusMsg:
		if msg.seq == m.loadSeq && m.snap != nil && msg.files != nil {
			m.snap.Head.Files, m.snap.Head.Dirty = msg.files, len(msg.files)
		}
		return nil

	case diffTickMsg:
		if msg.seq != m.diffSeq {
			return nil
		}
		return m.loadDiff(msg.spec)

	case diffLoadedMsg:
		if msg.err != nil {
			if msg.spec.key == m.dv.pending {
				m.dv = diffPanel{title: msg.spec.title, err: firstLine(msg.err.Error())}
			}
			return nil
		}
		if len(m.diffCache) > 128 {
			m.diffCache = map[string][]sbsRow{}
		}
		m.diffCache[msg.spec.key] = msg.rows
		if want, ok := m.wantDiff(); ok && want.key == msg.spec.key {
			m.setDiff(msg.spec, msg.rows)
		}
		return nil

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return tea.Quit
		}
		switch m.mode {
		case modeHelp:
			m.mode = modeNormal
			return nil
		case modeError:
			if s := msg.String(); s == "esc" || s == "enter" || s == "q" || s == " " {
				m.mode = modeNormal
			}
			return nil
		case modeConfirm:
			switch msg.String() {
			case "enter", "y":
				m.mode = modeNormal
				return m.confirm.run()
			case "esc", "n", "q":
				m.mode = modeNormal
			}
			return nil
		case modeMenu:
			return m.updateMenu(msg)
		case modeInput, modeSearch:
			return m.updateInput(msg)
		}
		if m.snap == nil {
			if msg.String() == "q" {
				return tea.Quit
			}
			return nil
		}
		if m.isMouseFragment(msg) {
			return nil
		}
		// Fast typing can deliver several runes in one KeyMsg; treat each as a keypress.
		if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 && !msg.Paste {
			var cmds []tea.Cmd
			for _, r := range msg.Runes {
				cmds = append(cmds, m.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}))
			}
			return tea.Batch(cmds...)
		}
		return m.handleKey(msg.String())
	}
	if m.mode == modeInput || m.mode == modeSearch {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return cmd
	}
	return nil
}

// isMouseFragment detects SGR mouse reports (ESC [ < b ; x ; y M) that reached
// us split across reads. bubbletea v1 then emits them as "alt+[" followed by
// plain runes, which would otherwise fire shortcuts like M (merge) or < (top).
func (m *Model) isMouseFragment(msg tea.KeyMsg) bool {
	if msg.Type != tea.KeyRunes {
		return false
	}
	if msg.Alt && string(msg.Runes) == "[" {
		m.mouseJunkUntil = time.Now().Add(150 * time.Millisecond)
		return true
	}
	s := string(msg.Runes)
	if len(msg.Runes) > 1 && strings.ContainsAny(s, ";<") {
		m.mouseJunkUntil = time.Now().Add(150 * time.Millisecond)
		return true
	}
	if time.Now().Before(m.mouseJunkUntil) && strings.Trim(s, "0123456789;<Mm") == "" {
		return true
	}
	return false
}

func (m *Model) updateMenu(msg tea.KeyMsg) tea.Cmd {
	k := msg.String()
	switch k {
	case "esc", "q":
		m.mode = modeNormal
		return nil
	case "j", "down":
		m.menu.cur = (m.menu.cur + 1) % len(m.menu.items)
		return nil
	case "k", "up":
		m.menu.cur = (m.menu.cur - 1 + len(m.menu.items)) % len(m.menu.items)
		return nil
	case "enter":
		m.mode = modeNormal
		return m.menu.items[m.menu.cur].run()
	}
	if k == " " {
		k = "space"
	}
	for _, it := range m.menu.items {
		if it.key == k {
			m.mode = modeNormal
			return it.run()
		}
	}
	return nil
}

func (m *Model) updateInput(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		if m.mode == modeSearch {
			m.applySearch("")
		}
		m.mode = modeNormal
		m.input.Blur()
		return nil
	case "enter":
		v := strings.TrimSpace(m.input.Value())
		mode := m.mode
		m.mode = modeNormal
		m.input.Blur()
		if mode == modeSearch {
			return nil // already applied while typing
		}
		if v == "" || m.inSubmit == nil {
			return nil
		}
		return m.inSubmit(v)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	if m.mode == modeSearch {
		m.applySearch(strings.TrimSpace(m.input.Value()))
	}
	return cmd
}

// applySearch runs on every keystroke: branches are filtered, other panels
// move to the first match at or after where the search started.
func (m *Model) applySearch(q string) {
	if m.searchPanel == pBranches {
		m.setBrFilter(q)
		return
	}
	m.search = q
	if q == "" {
		return
	}
	if m.searchPanel == pGraph {
		m.gCur = m.searchOrigin
	}
	m.searchFrom(0)
}

func (m *Model) handleMouse(msg tea.MouseMsg) tea.Cmd {
	if m.mode != modeNormal || m.snap == nil {
		return nil
	}
	l := m.layout()
	p := m.panelAt(msg.X, msg.Y)
	switch msg.Button {
	case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
		d := 3
		if msg.Button == tea.MouseButtonWheelUp {
			d = -3
		}
		if p == pDetail {
			m.scrollDetail(d) // scrolling must not change the selected file
			return nil
		}
		if p == pGraph {
			h := m.graphInnerHeight()
			m.gOff = clamp(m.gOff+d, 0, len(m.snap.Rows)-h)
			if m.gCur < m.gOff || m.gCur >= m.gOff+h {
				m.gCur = clamp(m.gCur, m.gOff, m.gOff+h-1)
				if !m.snap.Rows[m.gCur].Selectable() {
					m.moveGraph(1)
				}
			}
			return nil
		}
		m.moveList(p, d)
	case tea.MouseButtonLeft:
		if msg.Action != tea.MouseActionPress {
			return nil
		}
		y := msg.Y - 1 // header
		switch p {
		case pWorktrees:
			if i := m.wtOff + y - 1; y >= 1 && i < len(m.snap.Worktrees) {
				m.setFocus(pWorktrees)
				m.wtCur = i
				m.followList()
			}
		case pBranches:
			y -= l.wtH
			if y == 0 {
				m.clickBranchTab(msg.X)
				return nil
			}
			if i := m.brOff[m.brTab] + y - 1; y >= 1 && i < m.brLen(m.brTab) {
				m.setFocus(pBranches)
				m.brCur[m.brTab] = i
				m.followList()
			}
		case pGraph:
			if i := m.gOff + y - 1; y >= 1 && i < len(m.snap.Rows) && m.snap.Rows[i].Selectable() {
				m.setFocus(pGraph)
				m.gCur = i
			}
		case pDetail:
			m.setFocus(pDetail)
			w, h := m.detailSize()
			_, paths := m.detailLines(w)
			if i := m.detailOff + y - (l.wtH + l.brH) - 1; i >= 0 && i < len(paths) && paths[i] != "" {
				m.detailAnchor = ""
				m.selectDetailLine(i, paths, h)
			}
		case pDiff:
			m.setFocus(pDiff)
		}
	}
	return nil
}

// clickBranchTab maps a click on the branches box title ("[2] Local - Remotes - Tags")
// to a tab; the title starts after "╭─ ".
func (m *Model) clickBranchTab(x int) {
	pos := 3 + len("[2] ")
	for i, name := range brTabNames {
		if x >= pos && x < pos+len(name) {
			m.brTab = brTab(i)
			m.setFocus(pBranches)
			return
		}
		pos += len(name) + len(" - ")
	}
	m.setFocus(pBranches)
}

func (m *Model) handleKey(key string) tea.Cmd {
	m.status = ""

	// global
	switch key {
	case "q":
		return tea.Quit
	case "?", "x":
		m.mode = modeHelp
		return nil
	case "esc":
		switch {
		case m.detailAnchor != "":
			m.detailAnchor = ""
		case m.brFilter != "" && m.focus == pBranches:
			m.setBrFilter("")
		case m.search != "":
			m.search = ""
		case m.mark != nil:
			m.mark = nil
			m.setStatus("exited diff mode")
		case m.focus == pDiff || m.focus == pDetail:
			m.setFocus(m.listFocus)
		case m.zoomed:
			m.zoomed = false
			m.ensureListsVisible()
			m.ensureVisible()
		case len(m.cherry) > 0:
			m.cherry = nil
		}
		return nil
	case "tab", "l", "right":
		m.cycleFocus(1)
		return nil
	case "shift+tab", "h", "left":
		m.cycleFocus(-1)
		return nil
	case "1":
		m.setFocus(pWorktrees)
		return nil
	case "2":
		m.setFocus(pBranches)
		return nil
	case "3":
		m.setFocus(pGraph)
		return nil
	case "4":
		m.setFocus(pDetail)
		return nil
	case "5":
		m.setFocus(pDiff)
		return nil
	case "+":
		m.zoomed = !m.zoomed
		m.ensureListsVisible()
		return nil
	case "_":
		m.zoomed = false
		m.ensureListsVisible()
		return nil
	case "@":
		m.setFocus(pGraph)
		m.jumpToHead()
		return nil
	case "R":
		if m.focus != pBranches || m.brTab != tabLocal {
			m.setStatus("refreshed")
			return m.reload()
		}
	case "P":
		if m.focus != pBranches || m.brTab != tabTags {
			return m.push()
		}
	case "p":
		return m.gitOp("pull", opOpts{}, "pull")
	case "m":
		return m.mergeRebaseMenu()
	case "S":
		return m.stashMenu()
	case ":":
		return m.prompt("Shell command", "", func(c string) tea.Cmd { return m.execShell(c) })
	case "/":
		m.mode = modeSearch
		m.searchPanel = m.listFocus
		m.searchOrigin = m.gCur
		if m.searchPanel == pBranches {
			m.setFocus(pBranches)
			m.input.SetValue(m.brFilter)
		} else {
			m.input.SetValue(m.search)
		}
		m.input.CursorEnd()
		return m.input.Focus()
	case "J", "ctrl+d", "pgdown":
		m.scrollDiff(max(m.diffInnerHeight()/2, 1))
		return nil
	case "K", "ctrl+u", "pgup":
		m.scrollDiff(-max(m.diffInnerHeight()/2, 1))
		return nil
	case "H":
		m.scrollDiffX(-8)
		return nil
	case "L":
		m.scrollDiffX(8)
		return nil
	case "{":
		m.diffCtx = max(0, m.diffCtx-1)
		return nil
	case "}":
		m.diffCtx++
		return nil
	case "ctrl+w":
		m.diffIgnoreWS = !m.diffIgnoreWS
		return nil
	case "v":
		if m.focus == pDetail {
			m.toggleRangeSelect()
			return nil
		}
		m.diffUnified = !m.diffUnified
		m.applyDiffLayout()
		return nil
	case "W", "ctrl+e":
		if t, ok := m.listTarget(m.listFocus); ok {
			m.toggleDiffMark(t)
		}
		return nil
	}
	if m.search != "" && m.focus == m.searchPanel {
		switch key {
		case "n":
			m.searchNext(1)
			return nil
		case "N":
			m.searchNext(-1)
			return nil
		}
	}

	// navigation
	p := m.focus
	if p == pDetail {
		// j/k pick files; paging keys only scroll, keeping the selection.
		switch key {
		case ".", "pgdown":
			m.scrollDetail(m.pageSize(p))
			return nil
		case ",", "pgup":
			m.scrollDetail(-m.pageSize(p))
			return nil
		case "<", "home":
			m.scrollDetail(-1 << 30)
			return nil
		case ">", "end":
			m.scrollDetail(1 << 30)
			return nil
		}
	}
	switch key {
	case "j", "down":
		m.moveList(p, 1)
		return nil
	case "k", "up":
		m.moveList(p, -1)
		return nil
	case ".":
		m.moveList(p, m.pageSize(p))
		return nil
	case ",":
		m.moveList(p, -m.pageSize(p))
		return nil
	case "<", "home":
		m.moveList(p, -1<<30)
		return nil
	case ">", "end":
		m.moveList(p, 1<<30)
		return nil
	}

	if p == pDetail {
		switch key {
		case "enter":
			if m.detailSel != "" {
				m.setFocus(pDiff)
			} else {
				m.setFocus(m.listFocus)
			}
		case "d":
			return m.discardFile()
		case "shift+down", "shift+up":
			if m.detailAnchor == "" {
				m.detailAnchor = m.detailSel
			}
			m.moveDetailFile(map[string]int{"shift+down": 1, "shift+up": -1}[key])
		case " ":
			return m.toggleStageFile()
		case "a":
			return m.toggleStageAll()
		case "c":
			return m.commitStaged()
		}
		return nil
	}
	if p == pDiff {
		switch key {
		case "]":
			m.jumpFile(1)
		case "[":
			m.jumpFile(-1)
		case "t":
			if m.mark != nil {
				m.threeDot = !m.threeDot
			}
		case "w":
			m.diffNoWrap = !m.diffNoWrap
		case "enter":
			m.setFocus(m.listFocus)
		}
		return nil
	}
	if p == pBranches {
		switch key {
		case "[":
			m.brTab = (m.brTab + brTabCount - 1) % brTabCount
			m.followList()
			return nil
		case "]":
			m.brTab = (m.brTab + 1) % brTabCount
			m.followList()
			return nil
		}
	}

	t, ok := m.listTarget(p)
	if !ok {
		return nil
	}
	if key == "ctrl+o" {
		m.copyToClipboard(t)
		return nil
	}
	switch p {
	case pGraph:
		return m.graphKey(key, t)
	case pBranches:
		return m.branchKey(key, t)
	case pWorktrees:
		return m.worktreeKey(key, t)
	}
	return nil
}

func (m *Model) graphKey(key string, t target) tea.Cmd {
	switch key {
	case "enter":
		m.diffOpen = true
		m.setFocus(pDiff)
		m.listFocus = pGraph
	case " ":
		return m.checkout(t)
	case "n":
		return m.newBranch(t)
	case "w":
		return m.newWorktree(t)
	case "s":
		if t.kind == tkVirtual {
			return m.gitOp("stash", opOpts{}, "stash", "push")
		}
		return m.rewrite(t, "squash")
	case "f":
		return m.rewrite(t, "fixup")
	case "r":
		return m.rewrite(t, "reword")
	case "d":
		if t.kind == tkVirtual {
			m.discardMenu()
			return nil
		}
		return m.rewrite(t, "drop")
	case "e":
		return m.rewrite(t, "edit")
	case "i":
		if t.kind != tkCommit {
			return nil
		}
		base := t.hash + "^"
		if _, err := m.quick("rev-parse", "--verify", "-q", base); err != nil {
			base = "--root"
		}
		return m.execOp("interactive rebase", opOpts{}, nil, "rebase", "-i", "--autostash", base)
	case "c":
		m.toggleCherry(t)
	case "V":
		return m.pasteCherry()
	case "ctrl+r":
		m.cherry = nil
		m.setStatus("cherry-pick selection cleared")
	case "t":
		if t.kind != tkCommit {
			return nil
		}
		m.ask("Revert", fmt.Sprintf("Revert %s %q?", t.label, t.row.Subject), func() tea.Cmd {
			return m.gitOp("revert "+t.label, opOpts{jumpHead: true}, "revert", "--no-edit", t.hash)
		})
	case "T":
		if t.kind != tkCommit {
			return nil
		}
		return m.prompt("Tag name (at "+t.label+")", "", func(name string) tea.Cmd {
			return m.gitOp("tag "+name, opOpts{}, "tag", name, t.hash)
		})
	case "g":
		if t.kind == tkCommit {
			return m.resetMenu(t)
		}
	case "M":
		if t.kind == tkCommit {
			return m.mergeInto(t)
		}
	}
	return nil
}

func (m *Model) mergeInto(t target) tea.Cmd {
	m.ask("Merge", fmt.Sprintf("Merge %s into %s?", t.label, m.headLabel()), func() tea.Cmd {
		return m.gitOp("merge "+t.label, opOpts{jumpHead: true}, "merge", "--no-edit", t.ref)
	})
	return nil
}

func (m *Model) rebaseOnto(t target) tea.Cmd {
	if m.snap.Head.Branch == "" {
		m.showError("cannot rebase: detached HEAD")
		return nil
	}
	m.ask("Rebase", fmt.Sprintf("Rebase %s onto %s?", m.headLabel(), t.label), func() tea.Cmd {
		return m.gitOp("rebase onto "+t.label, opOpts{jumpHead: true}, "rebase", "--autostash", t.ref)
	})
	return nil
}

func (m *Model) branchKey(key string, t target) tea.Cmd {
	switch key {
	case " ":
		return m.checkout(t)
	case "enter":
		m.setFocus(pGraph)
		if i, ok := m.hashRow[t.hash]; ok {
			m.gCur = i
			m.ensureVisible()
		}
		return nil
	case "n":
		return m.newBranch(t)
	case "w":
		return m.newWorktree(t)
	case "d":
		return m.deleteMenu(t)
	case "r":
		return m.rebaseOnto(t)
	case "M":
		return m.mergeInto(t)
	case "g":
		return m.resetMenu(t)
	case "c":
		return m.prompt("Branch name to checkout", "", func(name string) tea.Cmd {
			return m.gitOp("checkout "+name, opOpts{jumpHead: true}, "switch", name)
		})
	case "-":
		return m.gitOp("checkout -", opOpts{jumpHead: true}, "switch", "-")
	case "f":
		switch t.kind {
		case tkLocal:
			return m.fastForward(t.local)
		case tkRemote:
			remote, _, _ := strings.Cut(t.remote.Name, "/")
			return m.gitOp("fetch "+remote, opOpts{}, "fetch", remote, "--prune")
		}
	case "R":
		if t.kind == tkLocal {
			old := t.local.Name
			return m.prompt("Rename branch "+old, old, func(name string) tea.Cmd {
				return m.gitOp("rename "+old, opOpts{}, "branch", "-m", old, name)
			})
		}
	case "P":
		if t.kind == tkTag {
			r := m.defaultRemote()
			if r == "" {
				m.showError("no remote configured")
				return nil
			}
			return m.gitOp("push tag "+t.tag.Name, opOpts{}, "push", r, "refs/tags/"+t.tag.Name)
		}
	case "T":
		return m.prompt("Tag name (at "+t.label+")", "", func(name string) tea.Cmd {
			return m.gitOp("tag "+name, opOpts{}, "tag", name, t.hash)
		})
	}
	return nil
}

func (m *Model) worktreeKey(key string, t target) tea.Cmd {
	switch key {
	case " ", "enter":
		return m.checkout(t)
	case "n":
		return m.newWorktree(t)
	case "d":
		return m.deleteMenu(t)
	case "f":
		return m.gitOp("fetch", opOpts{}, "fetch", "--all", "--prune")
	}
	return nil
}
