package ui

import (
	"strings"
)

// gridBand is one horizontal band of a shared-edge grid: h interior
// rows split into len(cols) panes of the given interior widths. The
// widths must sum to innerW-(len(cols)-1) — use splitCols.
type gridBand struct {
	h    int
	cols []int
}

// splitCols divides innerW cells among n panes separated by single
// dividers, weighted by weights (equal when nil). The last pane
// absorbs the rounding remainder so the widths always tile innerW.
func splitCols(innerW, n int, weights []int) []int {
	if n < 1 {
		return nil
	}
	avail := innerW - (n - 1)
	if avail < n {
		avail = n
	}
	out := make([]int, n)
	total := 0
	for i := 0; i < n; i++ {
		wt := 1
		if weights != nil {
			wt = weights[i]
			total += wt
		}
		out[i] = wt
	}
	if weights == nil {
		total = n
	}
	used := 0
	for i := 0; i < n-1; i++ {
		out[i] = avail * out[i] / total
		if out[i] < 1 {
			out[i] = 1
		}
		used += out[i]
	}
	out[n-1] = avail - used
	if out[n-1] < 1 {
		out[n-1] = 1
	}
	return out
}

// gridFrame draws a w-wide border grid for bands and returns it with
// the interior rect of every pane, indexed [band][col]. Like dashFrame
// it is one string with blank interiors for splicePane to fill, and
// drawing the whole grid at once is what yields ┬ ┴ ┼ junctions where
// adjacent bands' dividers meet or miss. The frame is 1 + Σh + len(bands)
// rows tall.
func gridFrame(w int, bands []gridBand, th Theme) (string, [][]rect) {
	innerW := w - 2
	var b strings.Builder
	rects := make([][]rect, len(bands))

	// edges reports the interior x offsets (0-based within innerW) of
	// a band's dividers.
	edges := func(cols []int) map[int]bool {
		out := map[int]bool{}
		x := 0
		for i, c := range cols {
			if i == len(cols)-1 {
				break
			}
			x += c
			out[x] = true
			x++
		}
		return out
	}
	rule := func(left, right rune, up, down map[int]bool) {
		row := make([]rune, 0, innerW+2)
		row = append(row, left)
		for x := 0; x < innerW; x++ {
			switch {
			case up[x] && down[x]:
				row = append(row, '┼')
			case up[x]:
				row = append(row, '┴')
			case down[x]:
				row = append(row, '┬')
			default:
				row = append(row, '─')
			}
		}
		row = append(row, right)
		b.WriteString(th.Dim.Render(string(row)))
	}

	y := 1
	for bi, band := range bands {
		if bi == 0 {
			rule('┌', '┐', nil, edges(band.cols))
		} else {
			rule('├', '┤', edges(bands[bi-1].cols), edges(band.cols))
		}
		b.WriteByte('\n')
		blank := "│"
		for _, c := range band.cols {
			blank += strings.Repeat(" ", c) + "│"
		}
		blank = th.Dim.Render(blank)
		for i := 0; i < band.h; i++ {
			b.WriteString(blank)
			b.WriteByte('\n')
		}
		x := 1
		for _, c := range band.cols {
			rects[bi] = append(rects[bi], rect{x: x, y: y, w: c, h: band.h})
			x += c + 1
		}
		y += band.h + 1
	}
	if len(bands) > 0 {
		rule('└', '┘', edges(bands[len(bands)-1].cols), nil)
	}
	return b.String(), rects
}
