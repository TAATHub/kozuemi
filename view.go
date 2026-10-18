package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = ansi.Truncate(s, w, "…")
	if pad := w - ansi.StringWidth(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}

// box draws a rounded border; title may carry its own styling.
func box(title string, lines []string, w, h int, focused bool) []string {
	if w < 4 || h < 2 {
		return nil
	}
	var bc lipgloss.TerminalColor = cBorder
	if focused {
		bc = cAccent
	}
	bs := lipgloss.NewStyle().Foreground(bc)
	iw, ih := w-2, h-2
	if !strings.Contains(title, "\x1b[") {
		title = lipgloss.NewStyle().Foreground(bc).Bold(focused).Render(title)
	}
	t := ansi.Truncate(" "+title+" ", iw-2, "…")
	rest := iw - 1 - ansi.StringWidth(t)
	out := make([]string, 0, h)
	out = append(out, bs.Render("╭─")+t+bs.Render(strings.Repeat("─", max(0, rest))+"╮"))
	for i := 0; i < ih; i++ {
		l := ""
		if i < len(lines) {
			l = lines[i]
		}
		out = append(out, bs.Render("│")+fit(l, iw)+bs.Render("│"))
	}
	out = append(out, bs.Render("╰"+strings.Repeat("─", iw)+"╯"))
	return out
}

type layoutInfo struct {
	leftW, rightW  int
	bodyH          int
	wtH, brH       int
	detailH        int // commit detail, bottom-left
	detailW, diffW int
	graphH, diffH  int
}

func (m *Model) layout() layoutInfo {
	var l layoutInfo
	if m.zoomed {
		// The focused pane takes the whole body; every other pane gets zero size.
		l.bodyH = max(m.height-2, 6)
		w, h := m.width, l.bodyH
		switch m.focus {
		case pWorktrees:
			l.leftW, l.wtH = w, h
		case pBranches:
			l.leftW, l.brH = w, h
		case pGraph:
			l.rightW, l.graphH = w, h
		case pDetail:
			l.detailW, l.detailH = w, h
		case pDiff:
			l.rightW, l.diffW, l.diffH = w, w, h
		}
		return l
	}
	l.leftW = clamp(m.width*22/100, 24, m.width/2)
	l.rightW = m.width - l.leftW
	l.bodyH = max(m.height-2, 6)
	nwt := 1
	if m.snap != nil {
		nwt = len(m.snap.Worktrees)
	}
	l.graphH = l.bodyH
	topH := l.bodyH
	if m.diffOpen {
		l.graphH = l.bodyH / 2
		l.detailH = l.bodyH - l.graphH
		topH = l.graphH
		l.diffH = l.bodyH - l.graphH
	}
	l.detailW, l.diffW = l.leftW, l.rightW
	l.wtH = clamp(nwt+2, 3, topH/3)
	l.brH = topH - l.wtH
	return l
}

func (m *Model) graphInnerHeight() int { return max(m.layout().graphH-2, 0) }
func (m *Model) diffInnerHeight() int  { return max(m.layout().diffH-2, 0) }
func (m *Model) diffInnerWidth() int   { return max(m.layout().diffW-2, 0) }

func (m *Model) panelAt(x, y int) panel {
	if m.zoomed {
		return m.focus
	}
	l := m.layout()
	y--
	if y >= l.wtH+l.brH {
		if x < l.detailW {
			return pDetail
		}
		return pDiff
	}
	if x < l.leftW {
		if y < l.wtH {
			return pWorktrees
		}
		return pBranches
	}
	if y < l.graphH {
		return pGraph
	}
	return pDiff
}

func (m *Model) View() string {
	if m.width == 0 {
		return ""
	}
	if m.snap == nil {
		if m.errText != "" {
			return lipgloss.NewStyle().Foreground(cRed).Render("error: "+m.errText) + "\n(q to quit)"
		}
		return "loading…"
	}
	if m.mode == modeHelp {
		return m.viewHelp()
	}
	l := m.layout()
	var body []string
	if m.zoomed {
		body = m.viewZoomed(l)
	} else {
		body = m.viewPanes(l)
	}
	switch m.mode {

	case modeConfirm:
		body = overlay(body, m.width, popup(m.confirm.title, []string{m.confirm.prompt, "", sDim.Render("enter/y: confirm   esc/n: cancel")}, m.width, cAccent))
	case modeInput:
		body = overlay(body, m.width, popup(m.inTitle, []string{m.input.View(), "", sDim.Render("enter: submit   esc: cancel")}, m.width, cAccent))
	case modeMenu:
		body = overlay(body, m.width, popup(m.menu.title, m.menuLines(), m.width, cAccent))
	case modeError:
		lines := append(strings.Split(m.errText, "\n"), "", sDim.Render("esc/enter: close"))
		body = overlay(body, m.width, popup("Error", lines, m.width, cRed))
	}
	return m.viewHeader() + "\n" + strings.Join(body, "\n") + "\n" + m.viewFooter()
}

// ---- popups ----

func popup(title string, lines []string, screenW int, color lipgloss.TerminalColor) []string {
	w := ansi.StringWidth(title) + 8
	for _, l := range lines {
		w = max(w, ansi.StringWidth(l)+4)
	}
	w = min(max(w, 44), screenW-4)
	bs := lipgloss.NewStyle().Foreground(color).Background(cPopBg)
	bg := lipgloss.NewStyle().Background(cPopBg)
	t := lipgloss.NewStyle().Foreground(color).Background(cPopBg).Bold(true).Render(" " + title + " ")
	out := []string{bs.Render("╭─") + t + bs.Render(strings.Repeat("─", max(0, w-3-ansi.StringWidth(title)-2))+"╮")}
	for _, l := range lines {
		for _, wl := range strings.Split(ansi.Wrap(l, w-4, ""), "\n") {
			inner := " " + wl
			pad := max(0, w-2-ansi.StringWidth(inner))
			out = append(out, bs.Render("│")+bg.Render(inner+strings.Repeat(" ", pad))+bs.Render("│"))
		}
	}
	return append(out, bs.Render("╰"+strings.Repeat("─", w-2)+"╯"))
}

func overlay(body []string, screenW int, pop []string) []string {
	if len(pop) == 0 {
		return body
	}
	pw := ansi.StringWidth(pop[0])
	x := max((screenW-pw)/2, 0)
	y := max((len(body)-len(pop))/2, 0)
	for i, p := range pop {
		if y+i >= len(body) {
			break
		}
		line := fit(body[y+i], screenW)
		body[y+i] = ansi.Truncate(line, x, "") + p + ansi.TruncateLeft(line, x+pw, "")
	}
	return body
}

func (m *Model) menuLines() []string {
	var out []string
	for i, it := range m.menu.items {
		key := fmt.Sprintf("%-6s", it.key)
		if i == m.menu.cur {
			out = append(out, lipgloss.NewStyle().Background(cSelBg).Render(key+" "+it.label))
			continue
		}
		out = append(out, lipgloss.NewStyle().Foreground(cCyan).Render(key)+" "+it.label)
	}
	return append(out, "", sDim.Render("press the key, or j/k + enter   esc: cancel"))
}

// ---- header / footer ----

func (m *Model) viewHeader() string {
	h := m.snap.Head
	parts := []string{
		lipgloss.NewStyle().Background(cAccent).Foreground(cBlack).Bold(true).Render(" gitscope "),
	}
	wtName := lipgloss.NewStyle().Foreground(cMagenta).Render(" (" + filepath.Base(h.Toplevel) + ")")
	if h.Branch != "" {
		parts = append(parts, lipgloss.NewStyle().Foreground(cGreen).Bold(true).Render("⎇ "+h.Branch)+wtName)
	} else if h.Hash != "" {
		parts = append(parts, lipgloss.NewStyle().Foreground(cYellow).Bold(true).Render("detached @ "+h.Hash[:7])+wtName)
	}
	if t := trackShort(h.Track); t != "" {
		parts = append(parts, lipgloss.NewStyle().Foreground(cCyan).Render(t))
	} else if h.Branch != "" && h.Upstream == "" {
		parts = append(parts, sDim.Render("(no upstream)"))
	}
	if h.Dirty > 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(cYellow).Render(fmt.Sprintf("✎ %d changed", h.Dirty)))
	}
	if h.State != "" {
		parts = append(parts, lipgloss.NewStyle().Background(cRed).Foreground(cBlack).Bold(true).Render(" "+h.State+" ")+
			sDim.Render(" m: options"))
	}
	if m.mark != nil {
		parts = append(parts, lipgloss.NewStyle().Foreground(cMagenta).Bold(true).Render("★ diffing from "+m.mark.label))
	}
	if n := len(m.cherry); n > 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(cCyan).Render(fmt.Sprintf("⎘ %d copied", n)))
	}
	return fit(strings.Join(parts, "  "), m.width)
}

func (m *Model) viewFooter() string {
	var s string
	switch {
	case m.mode == modeSearch:
		label := "Search: "
		if m.searchPanel == pBranches {
			label = "Filter: "
		}
		s = lipgloss.NewStyle().Foreground(cAccent).Bold(true).Render(label) + m.input.View()
	case m.busy != "":
		s = lipgloss.NewStyle().Foreground(cCyan).Render("⟳ " + m.busy + "…")
	case m.status != "":
		s = lipgloss.NewStyle().Foreground(cGreen).Render(m.status)
	case m.brFilter != "" && m.focus == pBranches:
		s = lipgloss.NewStyle().Foreground(cAccent).Render("filter: "+m.brFilter) + sDim.Render("  /: edit  esc: clear")
	case m.search != "":
		s = lipgloss.NewStyle().Foreground(cAccent).Render("search: "+m.search) + sDim.Render("  n/N: next/prev  esc: clear")
	default:
		s = sDim.Render(m.hints())
	}
	return fit(s, m.width)
}

func (m *Model) hints() string {
	switch m.focus {
	case pWorktrees:
		return "space: switch  n: new worktree  d: remove  f: fetch  W: diff mode  ?: keybindings"
	case pBranches:
		switch m.brTab {
		case tabRemote:
			return "space: checkout  n: new branch  M: merge  r: rebase  d: delete  f: fetch  [ ]: tab  ?: keybindings"
		case tabTags:
			return "space: checkout  d: delete  P: push tag  n: new branch  [ ]: tab  ?: keybindings"
		}
		return "space: checkout  n: new  d: delete  r: rebase  M: merge  f: fast-forward  R: rename  [ ]: tab  ?: keybindings"
	case pDetail:
		return "j/k: select  v/shift+↑↓: range  enter: diff  uncommitted → space: stage  a: all  c: commit  d: discard"
	case pDiff:
		return "j/k: scroll  ]/[: file  H/L: ←→  {/}: context  ctrl+w: whitespace  v: unified  w: wrap  +: maximize  esc: back"
	}
	return "enter: diff  space: checkout  c/V: cherry-pick  s/f/r/d: squash/fixup/reword/drop  W: diff mode  @: HEAD  ?: keybindings"
}

func trackShort(t string) string {
	if t == "" {
		return ""
	}
	if t == "gone" {
		return "✗gone"
	}
	var out []string
	for _, p := range strings.Split(t, ", ") {
		var n int
		if _, err := fmt.Sscanf(p, "ahead %d", &n); err == nil {
			out = append(out, fmt.Sprintf("↑%d", n))
		} else if _, err := fmt.Sscanf(p, "behind %d", &n); err == nil {
			out = append(out, fmt.Sprintf("↓%d", n))
		}
	}
	return strings.Join(out, "")
}

// shortAge turns git's "3 days ago" / "2 years, 1 month ago" into "3d" / "2y".
func shortAge(rel string) string {
	f := strings.Fields(rel)
	if len(f) < 2 {
		return rel
	}
	unit := strings.TrimSuffix(strings.TrimSuffix(f[1], ","), "s")
	abbr := map[string]string{"second": "s", "minute": "m", "hour": "h", "day": "d", "week": "w", "month": "mo", "year": "y"}
	if a, ok := abbr[unit]; ok {
		return f[0] + a
	}
	return rel
}

// ---- list panels ----

type listStyler struct {
	sel bool
	bg  lipgloss.TerminalColor
}

func (ls listStyler) st(s lipgloss.Style) lipgloss.Style {
	if ls.sel {
		return s.Background(ls.bg)
	}
	return s
}

func (m *Model) selBg(p panel) lipgloss.TerminalColor {
	if m.focus == p {
		return cSelBg
	}
	return cSelDim
}

func padSel(s string, w int, ls listStyler) string {
	s = ansi.Truncate(s, w, "…")
	if pad := w - ansi.StringWidth(s); pad > 0 {
		sp := strings.Repeat(" ", pad)
		if ls.sel {
			sp = lipgloss.NewStyle().Background(ls.bg).Render(sp)
		}
		s += sp
	}
	return s
}

func (m *Model) viewWorktrees(w, h int) []string {
	iw := w - 2
	wts := m.snap.Worktrees
	var lines []string
	for i := m.wtOff; i < len(wts) && len(lines) < h-2; i++ {
		wt := wts[i]
		ls := listStyler{sel: i == m.wtCur, bg: m.selBg(pWorktrees)}
		var name string
		if samePath(wt.Path, m.snap.Head.Toplevel) {
			name = ls.st(lipgloss.NewStyle().Foreground(cGreen).Bold(true)).Render("● " + wt.Name())
		} else {
			name = ls.st(lipgloss.NewStyle().Foreground(cMagenta)).Render("  " + wt.Name())
		}
		if wt.Prunable {
			name += ls.st(lipgloss.NewStyle().Foreground(cRed)).Render(" (missing)")
		}
		lines = append(lines, padSel(name, iw, ls))
	}
	return box(fmt.Sprintf("[1] Worktrees (%d)", len(wts)), lines, w, h, m.focus == pWorktrees)
}

func (m *Model) viewBranches(w, h int) []string {
	iw := w - 2
	var bc lipgloss.TerminalColor = cBorder
	if m.focus == pBranches {
		bc = cAccent
	}
	var tabs []string
	for i, name := range brTabNames {
		st := sDim
		if brTab(i) == m.brTab {
			st = lipgloss.NewStyle().Foreground(cAccent).Bold(true).Underline(m.focus == pBranches)
		}
		tabs = append(tabs, st.Render(name))
	}
	sep := lipgloss.NewStyle().Foreground(bc)
	title := sep.Render("[2] ") + strings.Join(tabs, sep.Render(" - ")) + sep.Render(m.brCountLabel())

	n := m.brLen(m.brTab)
	var lines []string
	for i := m.brOff[m.brTab]; i < n && len(lines) < h-2; i++ {
		ls := listStyler{sel: i == m.brCur[m.brTab], bg: m.selBg(pBranches)}
		lines = append(lines, m.branchLine(i, ls, iw))
	}
	if n == 0 {
		lines = append(lines, sDim.Render(" (none)"))
	}
	return box(title, lines, w, h, m.focus == pBranches)
}

func (m *Model) brCountLabel() string {
	total := [brTabCount]int{len(m.snap.Locals), len(m.snap.Remotes), len(m.snap.Tags)}[m.brTab]
	if m.brFilter != "" {
		return fmt.Sprintf(" (%d/%d)", m.brLen(m.brTab), total)
	}
	return fmt.Sprintf(" (%d)", total)
}

func (m *Model) branchLine(vi int, ls listStyler, w int) string {
	i := m.brView[m.brTab][vi]
	switch m.brTab {
	case tabLocal:
		b := m.snap.Locals[i]
		var name string
		switch {
		case b.Name == m.snap.Head.Branch:
			name = ls.st(lipgloss.NewStyle().Foreground(cGreen).Bold(true)).Render("● " + b.Name)
		case b.Worktree != "":
			name = ls.st(lipgloss.NewStyle().Foreground(cMagenta)).Render("  "+b.Name) +
				ls.st(sDim).Render(" ("+filepath.Base(b.Worktree)+")")
		default:
			name = ls.st(sPlain).Render("  " + b.Name)
		}
		status := ls.st(trackStyle(b)).Render(trackLabel(b))
		sw := ansi.StringWidth(status)
		name = padSel(name, max(w-sw-1, 1), ls)
		return padSel(name+ls.st(sPlain).Render(" ")+status, w, ls)
	case tabRemote:
		return padSel(ls.st(lipgloss.NewStyle().Foreground(cRed)).Render("  "+m.snap.Remotes[i].Name), w, ls)
	default:
		return padSel(ls.st(lipgloss.NewStyle().Foreground(cYellow)).Render("  "+m.snap.Tags[i].Name), w, ls)
	}
}

// trackLabel summarizes a local branch against its upstream: "✓" in sync,
// "↑1↓2" diverged, "✗ gone" when the remote branch was deleted (seen after a
// pruning fetch), "local" when it has no upstream.
func trackLabel(b Branch) string {
	switch {
	case b.Upstream == "":
		return "local"
	case b.Track == "gone":
		return "✗ gone"
	case b.Track == "":
		return "✓"
	}
	return trackShort(b.Track)
}

func trackStyle(b Branch) lipgloss.Style {
	switch {
	case b.Upstream == "":
		return sDim
	case b.Track == "gone":
		return lipgloss.NewStyle().Foreground(cRed)
	case b.Track == "":
		return lipgloss.NewStyle().Foreground(cGreen).Faint(true)
	}
	return lipgloss.NewStyle().Foreground(cCyan)
}

// ---- graph ----

func laneColor(i int) lipgloss.TerminalColor {
	if i == -2 {
		return cYellow
	}
	if i < 0 {
		return cDim
	}
	return lanePalette[i%len(lanePalette)]
}

func (m *Model) viewGraph(w, h int) []string {
	ih, iw := h-2, w-2
	end := min(len(m.snap.Rows), m.gOff+ih)
	graphW := 0
	for i := m.gOff; i < end; i++ {
		graphW = max(graphW, trimmedWidth(m.snap.Rows[i].Cells))
	}
	var lines []string
	for i := m.gOff; i < end; i++ {
		lines = append(lines, m.renderRow(i, iw, graphW))
	}
	if len(m.snap.Rows) == 0 {
		lines = append(lines, sDim.Render(" (no commits)"))
	}
	commits := 0
	for _, r := range m.snap.Rows {
		if r.IsCommit() {
			commits++
		}
	}
	pos := ""
	if commits > 0 {
		pos = fmt.Sprintf(" %d/%d", m.commitOrdinal(m.gCur), commits)
	}
	return box("[3] Commits"+pos, lines, w, h, m.focus == pGraph)
}

func (m *Model) commitOrdinal(row int) int {
	n := 0
	for i := 0; i <= row && i < len(m.snap.Rows); i++ {
		if m.snap.Rows[i].IsCommit() {
			n++
		}
	}
	return n
}

func trimmedWidth(cells []gcell) int {
	n := len(cells)
	for n > 0 && cells[n-1].ch == ' ' {
		n--
	}
	return n
}

// renderRow lays out: selection bar, graph, labels + subject, then right-aligned
// date / author / hash columns (dropped on narrow panes).
func (m *Model) renderRow(i, w, graphW int) string {
	row := m.snap.Rows[i]
	head := m.snap.Head
	hlOn := m.highlightActive() && row.IsCommit()
	ls := listStyler{sel: i == m.gCur, bg: m.selBg(pGraph)}
	if hlOn && ls.sel && row.Hash == m.hl.hash {
		ls.bg = cHlBg
	}
	st := ls.st
	sp := st(sPlain).Render(" ")

	var b strings.Builder
	switch {
	case ls.sel:
		b.WriteString(st(lipgloss.NewStyle().Foreground(cAccent)).Render("▌"))
	case hlOn && row.Hash == m.hl.hash:
		b.WriteString(lipgloss.NewStyle().Foreground(cMagenta).Bold(true).Render("┃"))
	case hlOn && m.hl.only[row.Hash]:
		b.WriteString(lipgloss.NewStyle().Foreground(cMagenta).Render("┃"))
	default:
		b.WriteString(" ")
	}
	b.WriteString(sp)

	isHead := row.IsCommit() && row.Hash == head.Hash
	otherWts := m.worktreeAt[row.Hash]
	for c := 0; c < graphW; c++ {
		cell := gcell{' ', -1}
		if c < len(row.Cells) {
			cell = row.Cells[c]
		}
		s := st(lipgloss.NewStyle().Foreground(laneColor(cell.color)))
		ch := cell.ch
		if ch == '●' {
			switch {
			case isHead:
				s = st(lipgloss.NewStyle().Foreground(cGreen).Bold(true))
			case len(otherWts) > 0:
				s = st(lipgloss.NewStyle().Foreground(cMagenta).Bold(true))
			case len(row.Parents) > 1:
				ch = '◉'
			default:
				ch = '○'
			}
		}
		b.WriteString(s.Render(string(ch)))
	}
	b.WriteString(sp + sp)

	if row.Virtual {
		b.WriteString(st(lipgloss.NewStyle().Foreground(cYellow)).Render(row.Subject))
		return padSel(b.String(), w, ls)
	}
	if !row.IsCommit() {
		return padSel(b.String(), w, ls)
	}

	m.writeLabels(&b, row, st, sp, laneColor(row.NodeColor), hlOn, otherWts, max(32, w*45/100))
	subj := sPlain
	if isHead {
		subj = subj.Bold(true)
	}
	b.WriteString(st(subj).Render(row.Subject))

	// Right-hand columns: author, hash, date (date dropped first when narrow).
	author := ansi.Truncate(row.Author, 10, "…")
	author += strings.Repeat(" ", 10-ansi.StringWidth(author))
	hashSt := lipgloss.NewStyle().Foreground(cYellow)
	if m.isCherry(row.Hash) {
		hashSt = lipgloss.NewStyle().Foreground(cBlack).Background(cCyan)
	}
	hash := st(hashSt).Render(row.Short)
	if m.mark != nil && m.mark.hash == row.Hash {
		hash = st(lipgloss.NewStyle().Foreground(cMagenta).Bold(true)).Render("★") + hash
	}
	cols := []string{st(lipgloss.NewStyle().Foreground(cCyan)).Render(author), hash, st(sDim).Render(row.Date)}
	left := b.String()
	leftMin := ansi.StringWidth(left) - ansi.StringWidth(row.Subject) + min(ansi.StringWidth(row.Subject), 24)
	for len(cols) > 1 {
		right := strings.Join(cols, st(sPlain).Render("  "))
		if w-ansi.StringWidth(right)-1 >= leftMin {
			break
		}
		cols = cols[:len(cols)-1]
	}
	right := strings.Join(cols, st(sPlain).Render("  "))
	lw := max(w-ansi.StringWidth(right)-1, 0)
	return padSel(left, lw, ls) + st(sPlain).Render(" ") + right
}

// writeLabels renders ref labels as colored badges: " ✓ main ↔ origin " for
// the checked-out branch, lane-colored badges for other branches, yellow for
// tags. Capped so the subject stays visible.
func (m *Model) writeLabels(b *strings.Builder, row Row, st func(lipgloss.Style) lipgloss.Style, sp string, lane lipgloss.TerminalColor, hlOn bool, otherWts []Worktree, maxW int) {
	head := m.snap.Head
	upstream := map[string]string{}
	gone := map[string]bool{}
	for _, l := range m.snap.Locals {
		if l.Upstream != "" {
			upstream[l.Name] = l.Upstream
		}
		gone[l.Name] = l.Track == "gone"
	}
	remotesHere := map[string]bool{}
	for _, r := range row.Refs {
		if r.Kind == RefRemote {
			remotesHere[r.Name] = true
		}
	}
	pill := func(bg lipgloss.TerminalColor) lipgloss.Style {
		return lipgloss.NewStyle().Background(bg).Foreground(cBlack)
	}

	const maxLabels = 3
	shown, hidden, used := 0, 0, 0
	emit := func(s lipgloss.Style, label, hlName string, force bool) {
		if hlOn && hlName == m.hl.label {
			s, force = s.Bold(true), true
		}
		if !force && (shown >= maxLabels || used > maxW) {
			hidden++
			return
		}
		shown++
		text := " " + ansi.Truncate(label, max(maxW-used, 12), "…") + " "
		used += ansi.StringWidth(text) + 1
		b.WriteString(s.Render(text))
		b.WriteString(sp)
	}

	absorbed := map[string]bool{}
	for _, r := range row.Refs {
		if r.Kind != RefLocal || m.branchInOtherWorktree(r.Name) {
			continue // other worktrees' branches are shown as worktree badges
		}
		label := r.Name
		if up := upstream[r.Name]; up != "" && remotesHere[up] {
			remote, _, _ := strings.Cut(up, "/")
			label += " ↔ " + remote
			absorbed[up] = true
		} else if gone[r.Name] {
			label += " ✗"
		}
		if r.Name == head.Branch {
			emit(pill(cGreen).Bold(true), "✓ "+label, r.Name, true)
			continue
		}
		emit(pill(lane), label, r.Name, false)
	}
	if row.Hash == head.Hash && head.Branch == "" {
		emit(pill(cGreen).Bold(true), "HEAD", "", true)
	}
	for _, wt := range otherWts {
		label := worktreeLabel(wt)
		if up := upstream[wt.Branch]; up != "" && remotesHere[up] {
			remote, _, _ := strings.Cut(up, "/")
			label = wt.Branch + " ↔ " + remote + " (" + wt.Name() + ")"
			absorbed[up] = true
		}
		emit(pill(cMagenta), label, worktreeLabel(wt), false)
	}
	for _, r := range row.Refs {
		switch {
		case r.Kind == RefRemote && !absorbed[r.Name]:
			emit(lipgloss.NewStyle().Background(cSelDim).Foreground(lane), r.Name, r.Name, false)
		case r.Kind == RefTag:
			emit(pill(cYellow), "◆ "+r.Name, r.Name, false)
		}
	}
	if hidden > 0 {
		b.WriteString(st(sDim).Render(fmt.Sprintf("+%d", hidden)))
		b.WriteString(sp)
	}
}

func (m *Model) branchInOtherWorktree(name string) bool {
	for _, wt := range m.snap.Worktrees {
		if wt.Branch == name && !samePath(wt.Path, m.snap.Head.Toplevel) {
			return true
		}
	}
	return false
}

// ---- help ----

const helpText = `gitscope keybindings (lazygit compatible)

 Global                                       Commits [3]        
   q            quit                            enter    focus diff panel (side-by-side)
   ? / x        keybindings                     space    checkout (branch at commit, else detached)
   h l ← → tab  previous / next panel           n  new branch          w  new worktree
   1 2 3 4 5    panels: worktrees/branches/commits/commit/diff   s  squash down         f  fixup
   j k , . < >  move / page / top / bottom      r  reword              d  drop
   / n N        search / next / previous        e  edit                i  interactive rebase
   R            refresh                         c  copy (cherry-pick)  V  paste    ctrl+r  clear
   P / p        push / pull                     t  revert              T  tag
   m            merge/rebase options            g  reset menu          M  merge commit
   S            stash options                   ctrl+o  copy hash
   :            run shell command               d / s on uncommitted: discard menu / stash
   W / ctrl+e   diff mode (mark → selection)  Branches [2]  Local - Remotes - Tags ([ ] tab)
   + / _        zoom focused pane / restore     space  checkout        enter  show in graph
   J K ctrl+d/u scroll diff   H L  ← →          n  new branch          w  new worktree
   { }          diff context − / +              d  delete menu         R  rename
   ctrl+w / v   whitespace / unified diff       r  rebase onto         M  merge into current
   @            jump to HEAD in graph           f  fast-forward / fetch remote
   esc          exit diff mode / un-maximize     g  reset menu          c  checkout by name
                                                -  checkout previous   P  push tag (Tags)
 Commit [4] / Diff [5]                          T  new tag             ctrl+o  copy name
   ] [          next / previous file
   t            toggle A..B / A...B (diff mode) Worktrees [1]
   w            toggle line wrap                space  switch          n  new worktree
                                                d  remove              f  fetch all
 Commit [4]
   v / shift+↑↓ range-select files (diff shows all selected)
 Commit [4] on uncommitted changes (acts on the selection)
   space / a    stage-unstage selection / all
   c            commit staged
   d            discard selection

 The graph always shows every ref in date order. Checking out or switching worktrees only moves
 the HEAD marker; graph order and scroll position stay put.
 ● HEAD (green) / other worktree HEAD (magenta)   ○ commit   ◉ merge commit   ◌ uncommitted changes

 press any key to close`

func (m *Model) viewHelp() string {
	lines := strings.Split(helpText, "\n")
	var out []string
	for i := 0; i < m.height && i < len(lines); i++ {
		out = append(out, fit(lines[i], m.width))
	}
	return strings.Join(out, "\n")
}

// worktreeLabel renders a worktree as "branch (worktree)", or "(worktree)" when detached.
func worktreeLabel(w Worktree) string {
	if w.Branch == "" {
		return "(" + w.Name() + ")"
	}
	return w.Branch + " (" + w.Name() + ")"
}

func (m *Model) viewPanes(l layoutInfo) []string {
	left := append(m.viewWorktrees(l.leftW, l.wtH), m.viewBranches(l.leftW, l.brH)...)
	if l.detailH > 0 {
		left = append(left, box("[4] Commit", m.renderDetailLines(l.detailW-2, l.detailH-2), l.detailW, l.detailH, m.focus == pDetail)...)
	}
	var right []string
	if l.graphH > 0 {
		right = m.viewGraph(l.rightW, l.graphH)
	}
	if l.diffH > 0 {
		right = append(right, box(m.diffBoxTitle(), m.renderDiffLines(l.diffW-2, l.diffH-2), l.diffW, l.diffH, m.focus == pDiff)...)
	}
	topH := l.wtH + l.brH
	body := make([]string, l.bodyH)
	for i := range body {
		var a, b string
		if i < len(left) {
			a = left[i]
		}
		if i < len(right) {
			b = right[i]
		}
		lw := l.leftW
		if i >= topH {
			lw = l.detailW
		}
		body[i] = fit(a, lw) + b
	}
	return body
}

func (m *Model) viewZoomed(l layoutInfo) []string {
	w, h := m.width, l.bodyH
	switch m.focus {
	case pWorktrees:
		return m.viewWorktrees(w, h)
	case pBranches:
		return m.viewBranches(w, h)
	case pDetail:
		return box("[4] Commit", m.renderDetailLines(w-2, h-2), w, h, true)
	case pDiff:
		return box(m.diffBoxTitle(), m.renderDiffLines(w-2, h-2), w, h, true)
	}
	return m.viewGraph(w, h)
}
