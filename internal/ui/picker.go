package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// renderNsPicker draws a centered modal listing namespaces. It does
// NOT consume the underlying screen — the caller composes it on top.
func (m Model) renderNsPicker(canvasWidth, canvasHeight int) string {
	if !m.nsPickerOpen {
		return ""
	}

	// Width: 32 cols, height up to 16 rows (incl. borders + title + hint).
	const w = 36
	const maxRows = 14

	options := m.nsPickerOptions
	visible := options
	if len(visible) > maxRows {
		// Window the list around the cursor.
		half := maxRows / 2
		start := m.nsPickerCursor - half
		if start < 0 {
			start = 0
		}
		end := start + maxRows
		if end > len(options) {
			end = len(options)
			start = end - maxRows
			if start < 0 {
				start = 0
			}
		}
		visible = options[start:end]
	}
	startIdx := 0
	if len(visible) > 0 && len(options) > maxRows {
		// Locate visible[0] in options to compute selected offset.
		for i, o := range options {
			if o == visible[0] {
				startIdx = i
				break
			}
		}
	}

	var b strings.Builder
	b.WriteString(m.Theme.Title.Render(" namespace ") + "\n")
	b.WriteString(m.Theme.Dim.Render(strings.Repeat("─", w-2)) + "\n")
	for i, opt := range visible {
		marker := "  "
		if i+startIdx == m.nsPickerCursor {
			marker = m.Theme.Title.Render(" ›")
		}
		text := truncate(opt, w-6)
		line := fmt.Sprintf("%s %s", marker, text)
		if i+startIdx == m.nsPickerCursor {
			line = renderSelected(line)
		}
		b.WriteString(line + "\n")
	}
	b.WriteString(m.Theme.Dim.Render(strings.Repeat("─", w-2)) + "\n")
	b.WriteString(m.Theme.Footer.Render(" j/k  enter  esc"))

	box := lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("244")).
		Padding(0, 1).
		Width(w).
		Render(b.String())

	// Center the box over the canvas. lipgloss.Place does the work.
	return lipgloss.Place(canvasWidth, canvasHeight, lipgloss.Center, lipgloss.Center, box)
}
