package ui

import "strings"

// column describes one table column for fitColumns: the narrowest
// width at which it's still useful (min must fit the header label
// plus a possible sort arrow), the widest width worth giving it, and
// how expendable it is when the pane can't hold everything.
type column struct {
	min  int
	max  int // == min for fixed-width columns
	prio int // 0 = never dropped; higher numbers are dropped first
}

// colGap is the blank run between adjacent visible columns.
const colGap = 2

// fitColumns resolves per-column widths for a pane avail cells wide.
// Columns are dropped highest-prio-first until the remaining minimum
// widths (plus gaps) fit, then leftover cells are dealt one at a
// time, round-robin in display order, to columns still below max.
// Dropped columns get width 0 — the pad helpers render those as ""
// and joinCells skips the gap.
func fitColumns(cols []column, avail int) []int {
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = c.min
	}
	total := func() int {
		t, n := 0, 0
		for _, w := range widths {
			if w > 0 {
				t, n = t+w, n+1
			}
		}
		if n > 1 {
			t += colGap * (n - 1)
		}
		return t
	}
	for total() > avail {
		drop, dropPrio := -1, 0
		for i, c := range cols {
			if widths[i] > 0 && c.prio > dropPrio {
				drop, dropPrio = i, c.prio
			}
		}
		if drop < 0 {
			break
		}
		widths[drop] = 0
	}
	leftover := avail - total()
	for leftover > 0 {
		grew := false
		for i, c := range cols {
			if leftover == 0 {
				break
			}
			if widths[i] > 0 && widths[i] < c.max {
				widths[i]++
				leftover--
				grew = true
			}
		}
		if !grew {
			break
		}
	}
	return widths
}

// joinCells joins already-padded cells with the column gap, skipping
// cells whose column was dropped (the pad helpers return "" for
// width <= 0).
func joinCells(cells ...string) string {
	var b strings.Builder
	for _, c := range cells {
		if c == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString(strings.Repeat(" ", colGap))
		}
		b.WriteString(c)
	}
	return b.String()
}
