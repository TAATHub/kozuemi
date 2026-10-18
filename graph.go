package main

import "fmt"

// gcell is one terminal column of the graph. Lane i owns column 2*i; the odd
// columns in between carry horizontal connectors.
type gcell struct {
	ch    rune
	color int
}

type lane struct {
	hash  string
	color int
}

type laneLayout struct {
	lanes     []lane
	nextColor int
}

func (l *laneLayout) newColor() int {
	c := l.nextColor
	l.nextColor++
	return c
}

func (l *laneLayout) freeSlot(from int) int {
	for i := max(from, 0); i < len(l.lanes); i++ {
		if l.lanes[i].hash == "" {
			return i
		}
	}
	l.lanes = append(l.lanes, lane{})
	return len(l.lanes) - 1
}

func (l *laneLayout) find(hash string) int {
	for i, ln := range l.lanes {
		if ln.hash == hash {
			return i
		}
	}
	return -1
}

func (l *laneLayout) trim() {
	for len(l.lanes) > 0 && l.lanes[len(l.lanes)-1].hash == "" {
		l.lanes = l.lanes[:len(l.lanes)-1]
	}
}

// pipes draws a vertical line for every active lane.
func (l *laneLayout) pipes() []gcell {
	cells := make([]gcell, 2*len(l.lanes))
	for i := range cells {
		cells[i] = gcell{' ', -1}
	}
	for i, ln := range l.lanes {
		if ln.hash != "" {
			cells[2*i] = gcell{'│', ln.color}
		}
	}
	return cells
}

func setCell(cells []gcell, pos int, c gcell) []gcell {
	for len(cells) <= pos {
		cells = append(cells, gcell{' ', -1})
	}
	cells[pos] = c
	return cells
}

// hline draws a horizontal connector strictly between columns a and b,
// crossing any vertical lane it passes.
func hline(cells []gcell, a, b, color int) []gcell {
	if a > b {
		a, b = b, a
	}
	for p := a + 1; p < b; p++ {
		if p < len(cells) && cells[p].ch == '│' {
			cells[p] = gcell{'┼', cells[p].color}
			continue
		}
		cells = setCell(cells, p, gcell{'─', color})
	}
	return cells
}

// layoutGraph assigns lanes to commits (newest first, as from --date-order)
// and inserts connector rows where several lanes converge on one commit.
func layoutGraph(commits []Row, headHash string, dirty int) []Row {
	var l laneLayout
	rows := make([]Row, 0, len(commits)+len(commits)/8)
	for _, c := range commits {
		var match []int
		for i, ln := range l.lanes {
			if ln.hash == c.Hash {
				match = append(match, i)
			}
		}
		col := -1
		if len(match) > 0 {
			col = match[0]
		} else {
			col = l.freeSlot(0)
			l.lanes[col] = lane{c.Hash, l.newColor()}
		}
		color := l.lanes[col].color

		if c.Hash == headHash && dirty > 0 {
			cells := l.pipes()
			cells = setCell(cells, 2*col, gcell{'◌', -2})
			rows = append(rows, Row{Cells: cells, Virtual: true, NodeColor: color,
				Subject: fmt.Sprintf("uncommitted changes (%d files)", dirty)})
		}

		if len(match) > 1 {
			// Branches that forked from this commit end here: ├─┴─╯
			cells := l.pipes()
			cells[2*col] = gcell{'├', color}
			last := match[len(match)-1]
			for _, j := range match[1:] {
				ch := '┴'
				if j == last {
					ch = '╯'
				}
				cells[2*j] = gcell{ch, l.lanes[j].color}
			}
			for k := 1; k < len(match); k++ {
				a, b := match[k-1], match[k]
				cells = hline(cells, 2*a, 2*b, l.lanes[b].color)
			}
			rows = append(rows, Row{Cells: cells})
			for _, j := range match[1:] {
				l.lanes[j] = lane{}
			}
		}

		cells := l.pipes()
		cells[2*col] = gcell{'●', color}
		var merged []string
		if len(c.Parents) > 0 {
			l.lanes[col].hash = c.Parents[0]
			merged = c.Parents[1:]
		} else {
			l.lanes[col] = lane{}
		}
		for _, p := range merged {
			k := l.find(p)
			existing := k >= 0
			if !existing {
				k = l.freeSlot(col + 1)
				l.lanes[k] = lane{p, l.newColor()}
			}
			pc := l.lanes[k].color
			cells = hline(cells, 2*col, 2*k, pc)
			var ch rune
			switch {
			case existing && k > col:
				ch = '┤'
			case existing:
				ch = '├'
			case k > col:
				ch = '╮'
			default:
				ch = '╭'
			}
			cells = setCell(cells, 2*k, gcell{ch, pc})
		}
		c.Cells = cells
		c.NodeColor = color
		rows = append(rows, c)
		l.trim()
	}
	return rows
}
