package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

type diffSpec struct {
	key   string
	title string
	args  []string
	// untracked appends untracked files as new-file diffs, which `git diff`
	// never includes on its own.
	untracked bool
}

type cellKind int

const (
	ckNone cellKind = iota
	ckCtx
	ckDel
	ckAdd
)

type sbsCell struct {
	num  int
	text string
	kind cellKind
}

type rowKind int

const (
	rkText rowKind = iota
	rkCommit
	rkStat
	rkFileStat
	rkSep
	rkFile
	rkMeta
	rkHunk
	rkPair
	rkUni // unified line: l and/or r set (both for context)
)

type sbsRow struct {
	kind rowKind
	text string
	l, r sbsCell
	// rkFileStat only
	status   string
	add, del int
}

type diffPanel struct {
	key     string
	detail  []sbsRow // commit header, message and file stat (shown in the left column)
	pending string
	title   string
	rows    []sbsRow // rows as displayed (sbs, or unified when toggled)
	sbs     []sbsRow
	vlines  []dline // visual lines of the selected file after wrapping
	vkey    string
	files   []int
	yOff    int
	xOff    int
	err     string
}

// dline is one screen line of the diff: segment seg of rows[row]; row -1 is
// the blank line between files.
type dline struct{ row, seg int }

type diffTickMsg struct {
	seq  int
	spec diffSpec
}

type diffLoadedMsg struct {
	spec diffSpec
	rows []sbsRow
	err  error
}

var (
	statRe = regexp.MustCompile(`^( .+\| +\d+ )(\+*)(-*)$`)
	hunkRe = regexp.MustCompile(`^@@+ -(\d+)(?:,\d+)? \+(\d+)`)
)

func parseSBS(raw string) []sbsRow {
	var rows []sbsRow
	lines := strings.Split(raw, "\n")
	var dels, adds []sbsCell
	oldN, newN := 0, 0
	inPatch, inHunk := false, false
	flush := func() {
		for i := 0; i < max(len(dels), len(adds)); i++ {
			r := sbsRow{kind: rkPair}
			if i < len(dels) {
				r.l = dels[i]
			}
			if i < len(adds) {
				r.r = adds[i]
			}
			rows = append(rows, r)
		}
		dels, adds = nil, nil
	}
	for _, l := range lines {
		if !inPatch && strings.HasPrefix(l, ":") && strings.Contains(l, "\t") {
			// --raw: ":100644 100644 abc def M\tpath" (renames carry old\tnew)
			meta, paths, _ := strings.Cut(l, "\t")
			f := strings.Fields(meta)
			ps := strings.Split(paths, "\t")
			if len(f) > 0 {
				rows = append(rows, sbsRow{kind: rkFileStat, status: f[len(f)-1][:1], text: ps[len(ps)-1]})
				continue
			}
		}
		l = strings.ReplaceAll(l, "\t", "    ")
		if strings.HasPrefix(l, "diff --git ") || strings.HasPrefix(l, "diff --cc ") {
			flush()
			inPatch, inHunk = true, false
			name := l
			if i := strings.LastIndex(l, " b/"); i >= 0 {
				name = l[i+3:]
			}
			rows = append(rows, sbsRow{kind: rkFile, text: name})
			continue
		}
		if !inPatch {
			if l == "---" {
				continue
			}
			kind := rkText
			switch {
			case strings.HasPrefix(l, "commit "):
				kind = rkCommit
			case statRe.MatchString(l):
				kind = rkStat
			}
			rows = append(rows, sbsRow{kind: kind, text: l})
			continue
		}
		if strings.HasPrefix(l, "@@") {
			flush()
			inHunk = true
			if mm := hunkRe.FindStringSubmatch(l); mm != nil {
				oldN, _ = strconv.Atoi(mm[1])
				newN, _ = strconv.Atoi(mm[2])
			}
			rows = append(rows, sbsRow{kind: rkHunk, text: l})
			continue
		}
		if !inHunk {
			for _, p := range []string{"new file", "deleted file", "rename from", "rename to", "Binary files", "old mode", "new mode"} {
				if strings.HasPrefix(l, p) {
					rows = append(rows, sbsRow{kind: rkMeta, text: l})
				}
			}
			continue
		}
		switch {
		case l == "":
			flush()
		case l[0] == '-':
			dels = append(dels, sbsCell{num: oldN, text: l[1:], kind: ckDel})
			oldN++
		case l[0] == '+':
			adds = append(adds, sbsCell{num: newN, text: l[1:], kind: ckAdd})
			newN++
		case l[0] == ' ':
			flush()
			rows = append(rows, sbsRow{kind: rkPair,
				l: sbsCell{num: oldN, text: l[1:], kind: ckCtx},
				r: sbsCell{num: newN, text: l[1:], kind: ckCtx}})
			oldN++
			newN++
		case l[0] == '\\':
		default:
			flush()
			inHunk = false
		}
	}
	flush()
	return countFileStats(rows)
}

// countFileStats fills +/- counts per file from the parsed patch and adds
// entries for untracked files, which --raw does not list.
func countFileStats(rows []sbsRow) []sbsRow {
	stat := map[string]int{}
	firstFile := len(rows)
	for i, r := range rows {
		if r.kind == rkFileStat {
			stat[r.text] = i
		}
		if r.kind == rkFile && i < firstFile {
			firstFile = i
		}
	}
	var extra []sbsRow
	cur := 1 << 30
	for _, r := range rows[firstFile:] {
		switch r.kind {
		case rkFile:
			name, untracked := strings.CutSuffix(r.text, " (untracked)")
			i, ok := stat[name]
			if !ok && untracked {
				extra = append(extra, sbsRow{kind: rkFileStat, status: "?", text: name})
				i = -(len(extra))
			} else if !ok {
				i = 1 << 30
			}
			cur = i
		case rkPair:
			var target *sbsRow
			switch {
			case cur >= 0 && cur < len(rows):
				target = &rows[cur]
			case cur < 0:
				target = &extra[-cur-1]
			}
			if target != nil {
				if r.l.kind == ckDel {
					target.del++
				}
				if r.r.kind == ckAdd {
					target.add++
				}
			}
		}
	}
	if len(extra) == 0 {
		return rows
	}
	out := append(append(append([]sbsRow{}, rows[:firstFile]...), extra...), rows[firstFile:]...)
	return out
}

// ---- loading ----

func (m *Model) diffArgs(base ...string) []string {
	args := append([]string{}, base...)
	args = append(args, "--no-color", "--no-ext-diff", fmt.Sprintf("-U%d", m.diffCtx))
	if m.diffIgnoreWS {
		args = append(args, "-w")
	}
	return args
}

// wantDiff decides what the diff panel should show for the current selection.
func (m *Model) wantDiff() (diffSpec, bool) {
	t, ok := m.listTarget(m.listFocus)
	if !ok {
		return diffSpec{}, false
	}
	// --raw gives full paths and A/M/D/R status for the file tree.
	stat := "--raw"
	opt := fmt.Sprintf("U%d,w%v", m.diffCtx, m.diffIgnoreWS)
	if m.mark != nil {
		sep := ".."
		if m.threeDot {
			sep = "..."
		}
		if t.kind == tkVirtual {
			return diffSpec{
				key:       "diff:" + m.mark.ref + ":wt:" + opt,
				title:     fmt.Sprintf("%s → working tree", m.mark.label),
				args:      append(m.diffArgs("diff", stat, "--shortstat", "--patch"), m.mark.ref),
				untracked: true,
			}, true
		}
		return diffSpec{
			key:   "diff:" + m.mark.ref + sep + t.ref + ":" + opt,
			title: fmt.Sprintf("%s%s%s", m.mark.label, sep, t.label),
			args:  append(m.diffArgs("diff", stat, "--shortstat", "--patch"), m.mark.ref+sep+t.ref),
		}, true
	}
	if t.kind == tkVirtual {
		return diffSpec{
			key:       "wt:" + opt,
			title:     "uncommitted changes (HEAD → working tree)",
			args:      append(m.diffArgs("diff", stat, "--shortstat", "--patch"), "HEAD"),
			untracked: true,
		}, true
	}
	if t.hash == "" {
		return diffSpec{}, false
	}
	return diffSpec{
		key:   "show:" + t.hash + ":" + opt,
		title: "commit " + t.hash[:min(10, len(t.hash))],
		args: append(m.diffArgs("show", stat, "--shortstat", "--patch", "--diff-merges=first-parent",
			"--date=format:%Y-%m-%d %H:%M:%S %z",
			"--format=commit %H%nAuthor: %an <%ae>%nDate:   %ad%nRefs:   %D%n%n%w(0,4,4)%B"), t.hash),
	}, true
}

func (m *Model) syncDiff() tea.Cmd {
	if !m.diffOpen || m.snap == nil {
		return nil
	}
	spec, ok := m.wantDiff()
	if !ok {
		m.dv = diffPanel{title: "(nothing to show)"}
		return nil
	}
	if spec.key == m.dv.key || spec.key == m.dv.pending {
		return nil
	}
	if rows, ok := m.diffCache[spec.key]; ok {
		m.setDiff(spec, rows)
		return nil
	}
	m.dv.pending = spec.key
	m.diffSeq++
	seq := m.diffSeq
	// Debounce so holding j/k doesn't spawn a git process per row.
	return tea.Tick(60*time.Millisecond, func(time.Time) tea.Msg { return diffTickMsg{seq: seq, spec: spec} })
}

func (m *Model) loadDiff(spec diffSpec) tea.Cmd {
	repo := m.repo
	return func() tea.Msg {
		out, err := repo.run(spec.args...)
		if err != nil {
			return diffLoadedMsg{spec: spec, err: err}
		}
		if spec.untracked {
			out += untrackedDiff(repo, spec.args)
		}
		if strings.TrimSpace(out) == "" {
			out = "(no differences)"
		}
		return diffLoadedMsg{spec: spec, rows: parseSBS(out)}
	}
}

func untrackedDiff(repo Repo, baseArgs []string) string {
	const maxFiles, maxSize = 100, 1 << 20
	list, err := repo.run("ls-files", "--others", "--exclude-standard", "-z")
	if err != nil || list == "" {
		return ""
	}
	files := strings.Split(strings.TrimRight(list, "\x00"), "\x00")
	var b strings.Builder
	// Reuse -U/-w options from the tracked diff.
	var opts []string
	for _, a := range baseArgs {
		if strings.HasPrefix(a, "-U") || a == "-w" {
			opts = append(opts, a)
		}
	}
	for i, f := range files {
		if i == maxFiles {
			fmt.Fprintf(&b, "diff --git a/… b/… (%d more untracked files not shown)\n", len(files)-maxFiles)
			break
		}
		if st, err := os.Stat(filepath.Join(repo.Dir, f)); err == nil && st.Size() > maxSize {
			fmt.Fprintf(&b, "diff --git a/%s b/%s (untracked)\nnew file (%d KB, not shown)\n", f, f, st.Size()/1024)
			continue
		}
		// --no-index exits 1 when files differ, which is always the case here.
		out, _ := repo.run(append(append([]string{"diff", "--no-index", "--no-color", "--no-ext-diff"}, opts...), "--", "/dev/null", f)...)
		// Tag the header so the file list shows which files are untracked.
		b.WriteString(strings.Replace(out, "\n", " (untracked)\n", 1))
	}
	return b.String()
}

func (m *Model) setDiff(spec diffSpec, rows []sbsRow) {
	split := len(rows)
	for i, r := range rows {
		if r.kind == rkFile {
			split = i
			break
		}
	}
	detail := rows[:split]
	for len(detail) > 0 && strings.TrimSpace(detail[len(detail)-1].text) == "" {
		detail = detail[:len(detail)-1]
	}
	// A blank row before every file after the first makes file boundaries clear.
	var patch []sbsRow
	for i, r := range rows[split:] {
		if r.kind == rkFile && i > 0 {
			patch = append(patch, sbsRow{kind: rkSep})
		}
		patch = append(patch, r)
	}
	// Reloading the same diff (e.g. after staging) keeps the file selection.
	same := spec.key == m.detailKey
	yOff := m.dv.yOff
	m.dv = diffPanel{key: spec.key, title: spec.title, detail: detail, sbs: patch}
	m.detailKey = spec.key
	files := m.treeFiles()
	has := func(p string) bool { return slices.Contains(files, p) }
	if same && has(m.detailSel) {
		if !has(m.detailAnchor) {
			m.detailAnchor = ""
		}
		m.dv.yOff = yOff
		m.applyDiffLayout()
		return
	}
	m.applyDiffLayout()
	m.detailOff = 0
	m.detailSel, m.detailAnchor = "", ""
	if len(files) > 0 {
		m.detailSel = files[0]
	}
}

// selectedFiles is the commit pane's selection in tree order: the range from
// the anchor to the cursor, or just the cursor file.
func (m *Model) selectedFiles() []string {
	if m.detailAnchor == "" {
		return m.selectedIn(nil)
	}
	return m.selectedIn(m.treeFiles())
}

func (m *Model) selectedIn(files []string) []string {
	if m.detailSel == "" {
		return nil
	}
	if m.detailAnchor == "" || m.detailAnchor == m.detailSel {
		return []string{m.detailSel}
	}
	a, b := slices.Index(files, m.detailAnchor), slices.Index(files, m.detailSel)
	if a < 0 || b < 0 {
		return []string{m.detailSel}
	}
	return files[min(a, b) : max(a, b)+1]
}

// applyDiffLayout derives the displayed rows from the parsed side-by-side
// rows; unified mode re-expands each change block to all deletions, then all
// additions, as `git diff` prints them.
func (m *Model) applyDiffLayout() {
	rows := m.dv.sbs
	if m.diffUnified {
		rows = make([]sbsRow, 0, len(m.dv.sbs)*3/2)
		var adds []sbsRow
		flush := func() {
			rows = append(rows, adds...)
			adds = nil
		}
		for _, r := range m.dv.sbs {
			switch {
			case r.kind != rkPair:
				flush()
				rows = append(rows, r)
			case r.l.kind == ckCtx:
				flush()
				rows = append(rows, sbsRow{kind: rkUni, l: r.l, r: r.r})
			default:
				if r.l.kind != ckNone {
					rows = append(rows, sbsRow{kind: rkUni, l: r.l})
				}
				if r.r.kind != ckNone {
					adds = append(adds, sbsRow{kind: rkUni, r: r.r})
				}
			}
		}
		flush()
	}
	var files []int
	for i, r := range rows {
		if r.kind == rkFile {
			files = append(files, i)
		}
	}
	m.dv.rows, m.dv.files = rows, files
	m.dv.vlines, m.dv.vkey = nil, ""
	m.scrollDiff(0)
}

// visualLines maps the selected file's rows to screen lines, splitting long
// lines when wrapping. When the width or wrap mode changes, the row at the top
// of the view stays at the top.
func (m *Model) visualLines(w int) []dline {
	ranges := m.fileRanges()
	key := fmt.Sprintf("%v:%d:%v", ranges, w, m.diffNoWrap)
	if key == m.dv.vkey {
		return m.dv.vlines
	}
	anchor := -1
	if m.dv.yOff < len(m.dv.vlines) && m.dv.vlines[m.dv.yOff].row >= 0 {
		anchor = m.dv.vlines[m.dv.yOff].row
	}
	var vl []dline
	for ri, rg := range ranges {
		if ri > 0 {
			vl = append(vl, dline{-1, 0})
		}
		for i := rg[0]; i < rg[1]; i++ {
			n := m.rowSegs(m.dv.rows[i], w)
			for k := range n {
				if i == anchor && k == 0 {
					m.dv.yOff = len(vl)
				}
				vl = append(vl, dline{i, k})
			}
		}
	}
	m.dv.vlines, m.dv.vkey = vl, key
	m.dv.yOff = clamp(m.dv.yOff, 0, len(vl)-m.diffInnerHeight())
	return vl
}

func pairWidths(w int) (int, int) {
	half := (w - 1) / 2
	return half, w - 1 - half
}

const (
	sbsNumW = 5  // "1234 "
	uniPreW = 12 // "1234 1234 + "
)

func (m *Model) rowSegs(r sbsRow, w int) int {
	if m.diffNoWrap {
		return 1
	}
	switch r.kind {
	case rkPair:
		lw, rw := pairWidths(w)
		return max(len(wrapText(r.l.text, lw-sbsNumW)), len(wrapText(r.r.text, rw-sbsNumW)))
	case rkUni:
		return len(wrapText(uniCell(r).text, w-uniPreW))
	}
	return 1
}

// wrapText splits s into chunks of at most w cells without breaking wide runes.
func wrapText(s string, w int) []string {
	if w < 1 || ansi.StringWidth(s) <= w {
		return []string{s}
	}
	var out []string
	var b strings.Builder
	cw := 0
	for _, r := range s {
		rw := ansi.StringWidth(string(r))
		if cw+rw > w && cw > 0 {
			out = append(out, b.String())
			b.Reset()
			cw = 0
		}
		b.WriteRune(r)
		cw += rw
	}
	return append(out, b.String())
}

// segment returns the part of text shown on segment k of a row.
func (m *Model) segment(text string, tw, k int) string {
	if m.diffNoWrap {
		return ansi.Cut(text, m.dv.xOff, m.dv.xOff+tw)
	}
	if segs := wrapText(text, tw); k < len(segs) {
		return segs[k]
	}
	return ""
}

// renderDetailLines draws the commit header, then changed files, then the
// message, so the file list stays visible even for long messages.
func (m *Model) renderDetailLines(w, h int) []string {
	out, _ := m.detailLines(w)
	m.detailOff = clamp(m.detailOff, 0, len(out)-h)
	out = out[min(m.detailOff, len(out)):]
	if len(out) > h {
		out = out[:h]
	}
	return out
}

func (m *Model) detailSize() (w, h int) {
	l := m.layout()
	return l.detailW - 2, l.detailH - 2
}

func (m *Model) scrollDetail(dy int) {
	w, h := m.detailSize()
	lines, _ := m.detailLines(w)
	m.detailOff = clamp(m.detailOff+dy, 0, len(lines)-h)
}

// moveDetailFile moves the file cursor in the commit pane's tree and scrolls
// the diff pane to that file. Without files it just scrolls.
func (m *Model) moveDetailFile(n int) {
	w, h := m.detailSize()
	_, paths := m.detailLines(w)
	var files []int // line indices of file entries
	cur := -1
	for i, p := range paths {
		if p != "" {
			if p == m.detailSel {
				cur = len(files)
			}
			files = append(files, i)
		}
	}
	if len(files) == 0 {
		m.scrollDetail(n)
		return
	}
	next := clamp(cur+n, 0, len(files)-1)
	if cur < 0 {
		next = 0
		if n < 0 {
			next = len(files) - 1
		}
	}
	if next == cur {
		// Already at the first/last file: scroll so the header (or the tail) stays reachable.
		m.scrollDetail(n)
		return
	}
	m.selectDetailLine(files[next], paths, h)
}

func (m *Model) selectDetailLine(line int, paths []string, h int) {
	m.detailSel = paths[line]
	if line < m.detailOff {
		m.detailOff = line
	}
	if line >= m.detailOff+h {
		m.detailOff = line - h + 1
	}
	m.jumpToFile(m.detailSel)
}

func (m *Model) jumpToFile(path string) {
	m.detailSel = path
	m.dv.yOff = 0
}

// fileRanges are the slices of diff rows (headers included) for the selected
// files, in diff order.
// The diff pane shows only this file.
func (m *Model) fileRanges() [][2]int {
	want := map[string]bool{}
	for _, f := range m.selectedFiles() {
		want[f] = true
	}
	var out [][2]int
	for i, r := range m.dv.rows {
		if r.kind != rkFile || !want[fileName(r)] {
			continue
		}
		end := len(m.dv.rows)
		for j := i + 1; j < len(m.dv.rows); j++ {
			if m.dv.rows[j].kind == rkFile {
				end = j
				break
			}
		}
		for end > i && m.dv.rows[end-1].kind == rkSep {
			end--
		}
		out = append(out, [2]int{i, end})
	}
	if out == nil {
		return [][2]int{{0, len(m.dv.rows)}}
	}
	return out
}

// treeFiles lists changed files in the commit pane's tree order.
func (m *Model) treeFiles() []string {
	w, _ := m.detailSize()
	_, paths := m.detailLines(w)
	var out []string
	for _, p := range paths {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// detailLines returns the rendered lines and, for file-tree lines, the file path.
func (m *Model) detailLines(w int) ([]string, []string) {
	if m.dv.err != "" {
		return []string{lipgloss.NewStyle().Foreground(cRed).Render(m.dv.err)}, []string{""}
	}
	label := func(k, v string, vs lipgloss.Style) string {
		return sDim.Render(fmt.Sprintf("%-7s", k)) + vs.Render(v)
	}
	var header, files, msg []string
	var stats []sbsRow
	for _, r := range m.dv.detail {
		t := r.text
		switch {
		case r.kind == rkCommit:
			hash := strings.TrimPrefix(t, "commit ")
			header = append(header, lipgloss.NewStyle().Foreground(cYellow).Render(hash))
		case strings.HasPrefix(t, "Author:"):
			header = append(header, label("Author", strings.TrimSpace(t[7:]), lipgloss.NewStyle().Foreground(cAccent)))
		case strings.HasPrefix(t, "Date:"):
			header = append(header, label("Date", strings.TrimSpace(t[5:]), sPlain))
		case strings.HasPrefix(t, "Refs:"):
			header = append(header, m.refLines(strings.TrimSpace(t[5:]), label)...)
		case r.kind == rkFileStat:
			stats = append(stats, r)
		case strings.Contains(t, " changed, ") || strings.HasSuffix(t, " changed"):
			// git's shortstat omits untracked files; the summary is rebuilt below.
		case strings.HasPrefix(t, "    "):
			for _, wl := range strings.Split(ansi.Wrap(t[4:], w, ""), "\n") {
				msg = append(msg, wl)
			}
		case strings.TrimSpace(t) == "":
			if len(msg) > 0 {
				msg = append(msg, "")
			}
		default:
			msg = append(msg, sDim.Render(t))
		}
	}
	if len(stats) > 0 {
		add, del := 0, 0
		for _, st := range stats {
			add, del = add+st.add, del+st.del
		}
		files = append(files, lipgloss.NewStyle().Bold(true).Render(fmt.Sprintf("%d files changed  ", len(stats)))+
			lipgloss.NewStyle().Foreground(cGreen).Render(fmt.Sprintf("+%d", add))+" "+
			lipgloss.NewStyle().Foreground(cRed).Render(fmt.Sprintf("-%d", del)))
	}
	var xy map[string]string
	if m.showingWorktree() {
		xy = m.snap.Head.Files
	}
	sel := map[string]bool{}
	if m.detailAnchor != "" {
		// Tree order is needed to resolve the range; a selection-free pass gives it.
		_, order := fileTree(stats, w, nil, nil)
		for _, f := range m.selectedIn(slices.DeleteFunc(order, func(p string) bool { return p == "" })) {
			sel[f] = true
		}
	} else {
		sel[m.detailSel] = true
	}
	tree, treePaths := fileTree(stats, w, sel, xy)
	files = append(files, tree...)
	for len(msg) > 0 && msg[len(msg)-1] == "" {
		msg = msg[:len(msg)-1]
	}
	var out, paths []string
	for _, block := range [][]string{header, msg, files} {
		if len(block) == 0 {
			continue
		}
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, block...)
	}
	paths = make([]string, len(out))
	// The tree is the last block, so its lines align with the tail of out.
	copy(paths[len(out)-len(treePaths):], treePaths)
	return out, paths
}

// refLines turns git's "%D" decoration ("HEAD -> main, origin/main, tag: v1")
// into one Branch/Tag entry per line.
func (m *Model) refLines(d string, label func(k, v string, vs lipgloss.Style) string) []string {
	var branches, tags []string
	for _, p := range strings.Split(d, ", ") {
		switch {
		case p == "", strings.HasSuffix(p, "/HEAD"):
		case p == "HEAD":
			branches = append(branches, lipgloss.NewStyle().Foreground(cGreen).Bold(true).Render("HEAD (detached)"))
		case strings.HasPrefix(p, "HEAD -> "):
			branches = append(branches, lipgloss.NewStyle().Foreground(cGreen).Bold(true).Render("✓ "+p[len("HEAD -> "):]))
		case strings.HasPrefix(p, "tag: "):
			tags = append(tags, lipgloss.NewStyle().Foreground(cYellow).Render(p[len("tag: "):]))
		case m.snap != nil && m.findLocal(p) != nil:
			branches = append(branches, lipgloss.NewStyle().Foreground(cAccent).Render(p))
		default:
			branches = append(branches, lipgloss.NewStyle().Foreground(cRed).Faint(true).Render(p))
		}
	}
	var out []string
	for _, group := range []struct {
		name  string
		items []string
	}{{"Branch", branches}, {"Tag", tags}} {
		for i, it := range group.items {
			k := group.name
			if i > 0 {
				k = ""
			}
			out = append(out, label(k, "", sPlain)+it)
		}
	}
	return out
}

func (m *Model) scrollDiff(dy int) {
	n := len(m.visualLines(m.diffInnerWidth()))
	m.dv.yOff = clamp(m.dv.yOff+dy, 0, n-m.diffInnerHeight())
}

func (m *Model) scrollDiffX(dx int) {
	if m.diffNoWrap {
		m.dv.xOff = max(0, m.dv.xOff+dx)
	}
}

// jumpFile selects the next/previous file in tree order (diff pane ] / [).
func (m *Model) jumpFile(dir int) {
	m.detailAnchor = ""
	files := m.treeFiles()
	for i, f := range files {
		if f == m.detailSel {
			if j := i + dir; j >= 0 && j < len(files) {
				w, h := m.detailSize()
				_, paths := m.detailLines(w)
				for li, p := range paths {
					if p == files[j] {
						m.selectDetailLine(li, paths, h)
					}
				}
			}
			return
		}
	}
}

// ---- rendering ----

func (m *Model) renderDiffLines(w, h int) []string {
	if m.dv.err != "" {
		return []string{lipgloss.NewStyle().Foreground(cRed).Render(m.dv.err)}
	}
	if m.dv.rows == nil {
		if m.dv.pending != "" {
			return []string{sDim.Render("loading…")}
		}
		return nil
	}
	vl := m.visualLines(w)
	var out []string
	for i := m.dv.yOff; i < len(vl) && len(out) < h; i++ {
		if vl[i].row < 0 {
			out = append(out, "")
			continue
		}
		out = append(out, m.renderDiffRow(m.dv.rows[vl[i].row], w, vl[i].seg))
	}
	return out
}

func fileName(r sbsRow) string {
	name, _ := strings.CutSuffix(r.text, " (untracked)")
	return name
}

func (m *Model) renderDiffRow(r sbsRow, w, seg int) string {
	x := m.dv.xOff
	switch r.kind {
	case rkSep:
		return ""
	case rkFile:
		return lipgloss.NewStyle().Background(cFileBg).Foreground(cYellow).Bold(true).Render(fit(" "+r.text, w))
	case rkMeta:
		return sDim.Render(r.text)
	case rkHunk:
		return lipgloss.NewStyle().Foreground(cCyan).Faint(true).Render(r.text)
	case rkCommit:
		return lipgloss.NewStyle().Foreground(cYellow).Bold(true).Render(r.text)
	case rkStat:
		if mm := statRe.FindStringSubmatch(r.text); mm != nil {
			return mm[1] + lipgloss.NewStyle().Foreground(cGreen).Render(mm[2]) + lipgloss.NewStyle().Foreground(cRed).Render(mm[3])
		}
		return r.text
	case rkPair:
		lw, rw := pairWidths(w)
		return m.sbsCellView(r.l, lw, seg) + sDim.Render("│") + m.sbsCellView(r.r, rw, seg)
	case rkUni:
		return m.uniLineView(r, w, seg)
	}
	return ansi.Cut(r.text, x, x+w)
}

func (m *Model) sbsCellView(c sbsCell, w, seg int) string {
	if w <= 0 {
		return ""
	}
	if c.kind == ckNone {
		return strings.Repeat(" ", w)
	}
	tw := max(w-sbsNumW, 0)
	text := m.segment(c.text, tw, seg)
	text += strings.Repeat(" ", max(0, tw-ansi.StringWidth(text)))
	num := fmt.Sprintf("%4d ", c.num)
	if seg > 0 {
		num = strings.Repeat(" ", sbsNumW)
	}
	ns, ts := sDim, sPlain
	switch c.kind {
	case ckDel:
		ns = ns.Background(cDelBg)
		ts = lipgloss.NewStyle().Foreground(cRed).Background(cDelBg)
	case ckAdd:
		ns = ns.Background(cAddBg)
		ts = lipgloss.NewStyle().Foreground(cGreen).Background(cAddBg)
	}
	return ansi.Truncate(ns.Render(num)+ts.Render(text), w, "")
}

func uniCell(r sbsRow) sbsCell {
	if r.l.kind == ckNone {
		return r.r
	}
	return r.l
}

func (m *Model) uniLineView(r sbsRow, w, seg int) string {
	num := func(c sbsCell) string {
		if c.kind == ckNone || seg > 0 {
			return "     "
		}
		return fmt.Sprintf("%4d ", c.num)
	}
	c, sign := uniCell(r), " "
	ns, ts := sDim, sPlain
	switch {
	case r.r.kind == ckNone:
		sign = "-"
		ns = ns.Background(cDelBg)
		ts = lipgloss.NewStyle().Foreground(cRed).Background(cDelBg)
	case r.l.kind == ckNone:
		sign = "+"
		ns = ns.Background(cAddBg)
		ts = lipgloss.NewStyle().Foreground(cGreen).Background(cAddBg)
	}
	if seg > 0 {
		sign = " "
	}
	prefix := num(r.l) + num(r.r) + sign + " "
	tw := max(w-uniPreW, 0)
	text := m.segment(c.text, tw, seg)
	text += strings.Repeat(" ", max(0, tw-ansi.StringWidth(text)))
	return ansi.Truncate(ns.Render(prefix)+ts.Render(text), w, "")
}

func (m *Model) diffBoxTitle() string {
	t := m.dv.title
	if m.mark != nil {
		t = "★ diff " + t
	}
	opts := []string{fmt.Sprintf("U%d", m.diffCtx)}
	if m.diffUnified {
		opts = append(opts, "unified")
	}
	if m.diffIgnoreWS {
		opts = append(opts, "-w")
	}
	if m.diffNoWrap {
		opts = append(opts, "nowrap")
	}
	if m.diffNoWrap && m.dv.xOff > 0 {
		opts = append(opts, fmt.Sprintf("→%d", m.dv.xOff))
	}
	if n := len(m.visualLines(m.diffInnerWidth())); n > 0 {
		opts = append(opts, fmt.Sprintf("%d/%d", min(m.dv.yOff+1, n), n))
	}
	head := "[5] Diff"
	if sel := m.selectedFiles(); len(sel) > 1 {
		head += fmt.Sprintf(" (%d files selected)", len(sel))
	} else if files := m.treeFiles(); len(files) > 0 {
		for i, f := range files {
			if f == m.detailSel {
				head += fmt.Sprintf(" (%d/%d files)", i+1, len(files))
			}
		}
	}
	return head + "  " + strings.Join(opts, " ")
}

type fileNode struct {
	name      string
	dirs      []*fileNode
	files     []sbsRow
	filePaths []string
	children  map[string]*fileNode
}

// fileTree renders changed files as a directory tree; chains of single-child
// directories are collapsed into one line (a/b/c/).
// fileTree renders changed files as a tree. With xy (working tree only), each
// file shows git's two-column status: index side green, worktree side red.
func fileTree(stats []sbsRow, w int, selected map[string]bool, xy map[string]string) ([]string, []string) {
	if len(stats) == 0 {
		return nil, nil
	}
	root := &fileNode{children: map[string]*fileNode{}}
	for _, st := range stats {
		parts := strings.Split(st.text, "/")
		n := root
		for _, p := range parts[:len(parts)-1] {
			c, ok := n.children[p]
			if !ok {
				c = &fileNode{name: p, children: map[string]*fileNode{}}
				n.children[p] = c
				n.dirs = append(n.dirs, c)
			}
			n = c
		}
		f := st
		f.text = parts[len(parts)-1]
		n.files = append(n.files, f)
		n.filePaths = append(n.filePaths, st.text)
	}
	var out, paths []string
	var walk func(n *fileNode, prefix string)
	walk = func(n *fileNode, prefix string) {
		total := len(n.dirs) + len(n.files)
		idx := 0
		branch := func() (string, string) {
			idx++
			if idx == total {
				return prefix + "└─ ", prefix + "   "
			}
			return prefix + "├─ ", prefix + "│  "
		}
		for _, d := range n.dirs {
			name := d.name
			for len(d.files) == 0 && len(d.dirs) == 1 {
				d = d.dirs[0]
				name += "/" + d.name
			}
			head, next := branch()
			out = append(out, sDim.Render(head)+lipgloss.NewStyle().Foreground(cAccent).Render(name+"/"))
			paths = append(paths, "")
			walk(d, next)
		}
		for i, f := range n.files {
			head, _ := branch()
			out = append(out, fileLine(sDim.Render(head), f, w, selected[n.filePaths[i]], xy[n.filePaths[i]]))
			paths = append(paths, n.filePaths[i])
		}
	}
	walk(root, "")
	return out, paths
}

func fileLine(prefix string, f sbsRow, w int, sel bool, xy string) string {
	stColor := map[string]lipgloss.TerminalColor{"A": cGreen, "M": cYellow, "D": cRed, "R": cCyan, "C": cCyan, "?": cGreen}[f.status]
	if stColor == nil {
		stColor = cDim
	}
	ls := listStyler{sel: sel, bg: cSelBg}
	sp := ls.st(sPlain).Render(" ")
	status := ls.st(lipgloss.NewStyle().Foreground(stColor).Bold(true)).Render(f.status)
	if len(xy) == 2 {
		x, y := cGreen, cRed
		if xy == "??" {
			x = cRed
		}
		status = ls.st(lipgloss.NewStyle().Foreground(x).Bold(true)).Render(xy[:1]) +
			ls.st(lipgloss.NewStyle().Foreground(y).Bold(true)).Render(xy[1:])
	}
	left := prefix + status + sp + ls.st(sPlain).Render(f.text)
	counts := ls.st(lipgloss.NewStyle().Foreground(cGreen)).Render(fmt.Sprintf("+%d", f.add)) + sp +
		ls.st(lipgloss.NewStyle().Foreground(cRed)).Render(fmt.Sprintf("-%d", f.del))
	cw := ansi.StringWidth(counts)
	lw := max(w-cw-1, 1)
	return padSel(left, lw, ls) + sp + counts
}
