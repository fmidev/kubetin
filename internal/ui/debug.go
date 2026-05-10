package ui

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// renderDebug renders the per-cluster diagnostic view shown when the
// user toggles debug mode (~). It deliberately shows untrimmed errors
// and exact timings — this view exists to debug kubetin, not to look
// pretty.
func (m Model) renderDebug(maxRows, maxWidth int) string {
	snap := m.Store.Snapshot()
	sort.Slice(snap, func(i, j int) bool {
		return sortKey(snap[i]) < sortKey(snap[j])
	})

	var b strings.Builder
	title := m.Theme.Title.Render(" debug · cluster diagnostics ")
	dim := m.Theme.Dim.Render(fmt.Sprintf(" press F2 to exit · pods in store: %d · focused: %s",
		len(m.pods), m.WatchedContext))
	b.WriteString(title)
	b.WriteByte('\n')
	b.WriteString(dim)
	b.WriteString("\n\n")

	const (
		colCtx   = 22
		colFile  = 18
		colReach = 12
		colVer   = 16
	)

	header := m.Theme.Header.Render(fmt.Sprintf(
		" %-1s  %-*s %-*s %-*s %-*s %4s %8s %8s",
		"",
		colCtx, "CONTEXT",
		colFile, "FILE",
		colReach, "REACH",
		colVer, "VERSION",
		"NODE", "LATENCY", "AGE",
	))
	b.WriteString(header)
	b.WriteByte('\n')

	now := time.Now()
	rendered := 4 // already wrote: title, dim, blank, header
	for _, st := range snap {
		if rendered >= maxRows {
			break
		}
		dot := m.Theme.styleForReach(st.Reach).Render(st.Reach.Glyph())
		ver := st.ServerVersion
		if ver == "" {
			ver = "—"
		}
		nodes := "—"
		if st.NodeCount >= 0 {
			nodes = fmt.Sprintf("%d", st.NodeCount)
		}
		latency := "—"
		if st.ProbeLatency > 0 {
			latency = fmt.Sprintf("%dms", st.ProbeLatency.Milliseconds())
		}
		age := "—"
		if !st.LastProbe.IsZero() {
			age = fmt.Sprintf("%ds", int(now.Sub(st.LastProbe).Seconds()))
		}
		marker := " "
		if st.Context == m.WatchedContext {
			marker = m.Theme.Title.Render("›")
		}
		ctxName := st.RawName
		if ctxName == "" {
			ctxName = st.Context
		}
		fileBase := ""
		if st.File != "" {
			fileBase = filepath.Base(st.File)
		}
		row := fmt.Sprintf(" %s %s %-*s %-*s %-*s %-*s %4s %8s %8s",
			marker, dot,
			colCtx, truncate(ctxName, colCtx),
			colFile, truncate(fileBase, colFile),
			colReach, st.Reach.String(),
			colVer, truncate(ver, colVer),
			nodes, latency, age)
		b.WriteString(row)
		b.WriteByte('\n')
		rendered++

		if st.LastError != "" {
			errLine := m.Theme.StatusBad.Render("       error: ") + truncate(st.LastError, maxWidth-14)
			b.WriteString(errLine)
			b.WriteByte('\n')
			rendered++
		}
	}
	return b.String()
}
