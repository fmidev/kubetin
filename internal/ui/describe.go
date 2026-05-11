package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/fmidev/kubetin/internal/cluster"
)

// DescribeRequestMsg asks main to fetch a describe for the given ref.
// (We tunnel through main because the supervisor lives there.)
type DescribeRequestMsg struct {
	Ref    cluster.DescribeRef
	Reveal bool
}

// DescribeResultMsg is the response — fetched YAML or an error.
type DescribeResultMsg cluster.DescribeResult

// describeState holds whatever we know about the open describe overlay.
type describeState struct {
	open    bool
	loading bool
	scroll  int
	result  cluster.DescribeResult
	// revealed is set to true when the user pressed Shift-Y in-modal to
	// re-fetch a Secret with reveal=true. The banner this drives is
	// persistent (unlike the redacted hint, which only shows while
	// values were actually redacted) so the user can never miss the
	// fact that they're looking at plaintext credentials. Cleared on
	// close along with the YAML buffer.
	revealed bool
}

// refForCursor returns the DescribeRef for the currently-selected row
// in the active view, or false if there's no valid selection.
func (m Model) refForCursor() (cluster.DescribeRef, bool) {
	if m.cursor == "" {
		return cluster.DescribeRef{}, false
	}
	switch m.view {
	case ViewPods:
		if r, ok := m.pods[m.cursor]; ok {
			return cluster.DescribeRef{
				Version: "v1", Resource: "pods", Kind: "Pod",
				Namespace: r.Namespace, Name: r.Name,
			}, true
		}
	case ViewDeployments:
		if r, ok := m.deployments[m.cursor]; ok {
			return cluster.DescribeRef{
				Group: "apps", Version: "v1", Resource: "deployments", Kind: "Deployment",
				Namespace: r.Namespace, Name: r.Name,
			}, true
		}
	case ViewNodes:
		if r, ok := m.nodes[m.cursor]; ok {
			return cluster.DescribeRef{
				Version: "v1", Resource: "nodes", Kind: "Node",
				Name: r.Name,
			}, true
		}
	case ViewNamespaces:
		if r, ok := m.namespaces[m.cursor]; ok {
			return cluster.DescribeRef{
				Version: "v1", Resource: "namespaces", Kind: "Namespace",
				Name: r.Name,
			}, true
		}
	}
	return cluster.DescribeRef{}, false
}

// renderDescribe draws the centred describe overlay.
func (m Model) renderDescribe(canvasWidth, canvasHeight int) string {
	w := canvasWidth - 8
	if w > 120 {
		w = 120
	}
	if w < 50 {
		w = 50
	}
	h := canvasHeight - 4
	if h < 10 {
		h = 10
	}

	var b strings.Builder
	title := m.Theme.Title.Render(" describe ") +
		m.Theme.Dim.Render(" "+describeTitle(m.describe.result))
	b.WriteString(title)
	b.WriteByte('\n')
	b.WriteString(m.Theme.Dim.Render(strings.Repeat("─", w-2)))
	b.WriteString("\n\n")

	if m.describe.loading {
		b.WriteString(m.Theme.Dim.Render(" loading…"))
	} else if m.describe.result.Err != "" {
		b.WriteString(m.Theme.StatusBad.Render(" error: " + m.describe.result.Err))
	} else {
		body := m.describe.result.YAML
		lines := strings.Split(body, "\n")
		viewable := h - 6 // title + sep + blank + footer + redacted notice
		start := m.describe.scroll
		if start > len(lines)-viewable {
			start = len(lines) - viewable
		}
		if start < 0 {
			start = 0
		}
		end := start + viewable
		if end > len(lines) {
			end = len(lines)
		}
		for _, ln := range lines[start:end] {
			b.WriteString(truncate(ln, w-4) + "\n")
		}
	}

	b.WriteString("\n")
	switch {
	case m.describe.revealed && m.describe.result.Ref.Kind == "Secret":
		// Bright, persistent: this YAML contains plaintext credentials.
		// Keep it on screen for the entire reveal session — never let a
		// user forget what's on their screen while they scroll.
		b.WriteString(m.Theme.StatusBad.Render(" ⚠ REVEALED — Secret values are in cleartext. Reveal logged to debug.log."))
		b.WriteByte('\n')
	case m.describe.result.Redacted:
		b.WriteString(m.Theme.StatusWrn.Render(" Secret values redacted. Press Shift-Y to reveal (logged to debug.log)."))
		b.WriteByte('\n')
	}
	// ConfigMaps frequently carry credentials despite the "config not
	// secret" convention — DB hosts, mTLS certs, integration tokens.
	// We can't redact them (no shape to redact against), but we should
	// not let a user paste this into a ticket without thinking.
	if m.describe.result.Ref.Kind == "ConfigMap" && m.describe.result.Err == "" && !m.describe.loading {
		b.WriteString(m.Theme.StatusWrn.Render(" ConfigMap values are NOT redacted — review before sharing or pasting."))
		b.WriteByte('\n')
	}
	b.WriteString(m.Theme.Footer.Render(" j/k scroll  g/G top/bot  Esc close"))

	box := lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("244")).
		Padding(0, 2).
		Width(w).
		Render(b.String())

	return lipgloss.Place(canvasWidth, canvasHeight, lipgloss.Center, lipgloss.Center, box)
}

func describeTitle(r cluster.DescribeResult) string {
	if r.Ref.Name == "" {
		return ""
	}
	if r.Ref.Namespace != "" {
		return r.Ref.Kind + "/" + r.Ref.Name + " · " + r.Ref.Namespace
	}
	return r.Ref.Kind + "/" + r.Ref.Name
}
