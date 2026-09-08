package ui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/fmidev/kubetin/internal/cluster"
)

// drainConfirmState holds the confirmation before drain preflight.
type drainConfirmState struct {
	open    bool
	node    string
	pending bool // true while waiting for the drain start handshake
}

// drainProgressState backs the live progress modal that opens once
// the drain is dispatched. It absorbs the stream of cluster.DrainProgress
// events the supervisor sends down a channel via DrainProgressMsg.
//
// "blocked" pods (failed eviction requests) accumulate in a list
// so the user can see exactly what's stuck rather than just a count;
// successfully-evicted pods just bump the Done counter.
type drainProgressState struct {
	open      bool
	context   string // origin cluster, for the Tab-away guard
	node      string
	current   string // pod currently being evicted
	done      int
	total     int
	blocked   []string // ns/name and error for pods whose eviction failed
	remaining []string
	phase     string // mirrors cluster.DrainProgress.Phase
	err       string
	cancel    func()
	started   time.Time
}

// DrainStartMsg / DrainProgressMsg / DrainDoneMsg are the lifecycle
// of one drain operation. Start surfaces a fatal setup error (RBAC
// or kubeconfig failure before the first evict); Progress is one
// event per pod / phase transition; Done closes a successful drain
// or leaves an incomplete result open for inspection.
type DrainStartMsg struct {
	Context string
	Node    string
	Err     string // non-empty means we never started the eviction loop
	Cancel  func()
}

type DrainProgressMsg cluster.DrainProgress

type DrainDoneMsg struct {
	Context   string
	Node      string
	Done      int
	Total     int
	Err       string
	Blocked   []string
	Remaining []string
}

// openDrainConfirm shows the y/N modal for the given node. The
// actual drain doesn't start until the user confirms.
func (m Model) openDrainConfirm(ref cluster.DescribeRef) (tea.Model, tea.Cmd) {
	m.drainConfirm.open = true
	m.drainConfirm.node = ref.Name
	m.drainConfirm.pending = false
	return m, nil
}

// handleDrainConfirmKey routes input while the confirm modal is open.
func (m Model) handleDrainConfirmKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.drainConfirm.pending {
		// In-flight: ignore everything except Esc (still allow
		// cancelling before the supervisor accepts).
		if k.String() == "esc" || k.String() == "q" {
			m.drainConfirm.open = false
			m.drainConfirm.pending = false
		}
		return m, nil
	}
	switch k.String() {
	case "esc", "q", "n", "N":
		m.drainConfirm.open = false
		return m, nil
	case "ctrl+c":
		m.quitMsg = "bye"
		return m, tea.Quit
	case "y", "Y", "enter":
		if m.OnDrainStart == nil {
			m.drainConfirm.open = false
			return m, nil
		}
		m.drainConfirm.pending = true
		cb := m.OnDrainStart
		node := m.drainConfirm.node
		focused := m.WatchedContext
		return m, func() tea.Msg { return cb(focused, node) }
	}
	return m, nil
}

// handleDrainProgressKey routes input while the progress modal is
// open. The only key it accepts is Esc, which cancels the drain
// via the supervisor's context. Pods already evicted stay evicted —
// cancellation just stops further evictions.
func (m Model) handleDrainProgressKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc", "q":
		if m.drainProgress.phase == "done" || m.drainProgress.phase == "error" {
			m.drainProgress.open = false
			return m, nil
		}
		if m.drainProgress.cancel != nil {
			m.drainProgress.cancel()
		}
		return m, nil
	case "ctrl+c":
		m.quitMsg = "bye"
		return m, tea.Quit
	}
	return m, nil
}

// applyDrainStart is the handler for DrainStartMsg. On success it
// opens the progress modal and remembers the cancel function. On
// failure it surfaces a toast and closes the confirm modal.
func (m Model) applyDrainStart(msg DrainStartMsg) (tea.Model, tea.Cmd) {
	if msg.Context != m.WatchedContext {
		return m, nil
	}
	m.drainConfirm.open = false
	m.drainConfirm.pending = false
	if msg.Err != "" {
		m.toast = "✕ Drain: " + cleanDetail(msg.Err)
		m.toastUntil = time.Now().Add(5 * time.Second)
		return m, tea.Tick(5*time.Second, func(t time.Time) tea.Msg { return toastClearMsg(t) })
	}
	m.drainProgress = drainProgressState{
		open:    true,
		context: msg.Context,
		node:    msg.Node,
		cancel:  msg.Cancel,
		started: time.Now(),
		phase:   "starting",
	}
	return m, nil
}

// applyDrainProgress folds one DrainProgressMsg into drainProgressState.
// Out-of-context messages are dropped (cluster Tab guard pattern).
func (m Model) applyDrainProgress(msg DrainProgressMsg) (tea.Model, tea.Cmd) {
	if msg.Context != m.drainProgress.context {
		return m, nil
	}
	m.drainProgress.phase = msg.Phase
	if msg.Total > 0 {
		m.drainProgress.total = msg.Total
	}
	if msg.Done > m.drainProgress.done {
		m.drainProgress.done = msg.Done
	}
	switch msg.Phase {
	case "evicting", "accepted":
		m.drainProgress.current = msg.Pod
	case "blocked":
		m.drainProgress.blocked = append(m.drainProgress.blocked,
			msg.Pod+" ("+cleanDetail(msg.Err)+")")
	}
	return m, nil
}

// applyDrainDone retains incomplete results until the user dismisses them.
func (m Model) applyDrainDone(msg DrainDoneMsg) (tea.Model, tea.Cmd) {
	if msg.Context != m.WatchedContext {
		return m, nil
	}
	m.drainProgress.open = false
	m.drainProgress.cancel = nil
	if msg.Total > msg.Done {
		m.drainProgress.open = true
		m.drainProgress.node = msg.Node
		m.drainProgress.phase = "done"
		m.drainProgress.done = msg.Done
		m.drainProgress.total = msg.Total
		m.drainProgress.err = cleanDetail(msg.Err)
		blockedDetails := make(map[string]string, len(msg.Blocked))
		for _, detail := range msg.Blocked {
			pod, _, _ := strings.Cut(detail, " (")
			blockedDetails[pod] = cleanDetail(detail)
		}
		m.drainProgress.remaining = make([]string, len(msg.Remaining))
		for i, pod := range msg.Remaining {
			m.drainProgress.remaining[i] = cleanDetail(pod)
			if detail, ok := blockedDetails[pod]; ok {
				m.drainProgress.remaining[i] = detail
			}
		}
	}
	if msg.Err != "" {
		if msg.Total == 0 {
			m.toast = fmt.Sprintf("✕ Drain %s: %s", msg.Node, cleanDetail(msg.Err))
		} else {
			m.toast = fmt.Sprintf("✕ Drain %s: %d/%d terminated; %s", msg.Node, msg.Done, msg.Total, cleanDetail(msg.Err))
		}
	} else if len(msg.Blocked) > 0 {
		m.toast = fmt.Sprintf("⚠ Drain %s incomplete: %d/%d (%d blocked)",
			msg.Node, msg.Done, msg.Total, len(msg.Blocked))
	} else if msg.Done < msg.Total {
		m.toast = fmt.Sprintf("⚠ Drain %s incomplete: %d/%d terminated", msg.Node, msg.Done, msg.Total)
	} else {
		m.toast = fmt.Sprintf("✓ Drained %s: %d/%d", msg.Node, msg.Done, msg.Total)
	}
	m.toastUntil = time.Now().Add(6 * time.Second)
	return m, tea.Tick(6*time.Second, func(t time.Time) tea.Msg { return toastClearMsg(t) })
}

// renderDrainConfirm draws the y/N confirmation.
func (m Model) renderDrainConfirm(canvasWidth, canvasHeight int) string {
	if !m.drainConfirm.open {
		return ""
	}
	const w = 60
	var b strings.Builder
	title := m.Theme.Title.Render(" Drain node ") +
		m.Theme.Dim.Render(" "+m.drainConfirm.node)
	b.WriteString(title + "\n")
	b.WriteString(m.Theme.Dim.Render(strings.Repeat("─", w-2)) + "\n\n")
	b.WriteString(" This will cordon the node, then check all candidates.\n")
	b.WriteString(" No pods are evicted if any candidate uses emptyDir\n")
	b.WriteString(" or its controller cannot be verified. The node stays\n")
	b.WriteString(" cordoned if checks fail. Mirror, DaemonSet-owned,\n")
	b.WriteString(" and completed pods are skipped. PDBs are respected.\n")
	b.WriteString(" Waits for pod termination; stops after 10 minutes.\n\n")
	if m.drainConfirm.pending {
		b.WriteString(m.Theme.StatusWrn.Render(" starting…") + "\n")
	} else {
		b.WriteString(m.Theme.StatusBad.Render(" y") +
			m.Theme.Base.Render(" to drain   ") +
			m.Theme.Base.Render("n / esc to cancel") + "\n")
	}
	return m.boxed(b.String(), w, canvasWidth, canvasHeight)
}

// renderDrainProgress draws the live progress modal.
func (m Model) renderDrainProgress(canvasWidth, canvasHeight int) string {
	if !m.drainProgress.open {
		return ""
	}
	const w = 70
	var b strings.Builder
	title := m.Theme.Title.Render(" Draining ") +
		m.Theme.Dim.Render(" "+m.drainProgress.node)
	b.WriteString(title + "\n")
	b.WriteString(m.Theme.Dim.Render(strings.Repeat("─", w-2)) + "\n\n")

	switch m.drainProgress.phase {
	case "starting":
		b.WriteString(" cordoning + checking pods and controllers…\n")
	case "evicting":
		fmt.Fprintf(&b, " %d / %d terminated    %s\n",
			m.drainProgress.done, m.drainProgress.total,
			m.Theme.Dim.Render("evicting "+m.drainProgress.current))
	case "waiting":
		fmt.Fprintf(&b, " %d / %d terminated\n", m.drainProgress.done, m.drainProgress.total)
		b.WriteString(m.Theme.Dim.Render(" waiting for pod termination") + "\n")
	case "accepted":
		fmt.Fprintf(&b, " %d / %d terminated\n", m.drainProgress.done, m.drainProgress.total)
		b.WriteString(m.Theme.Dim.Render(" eviction accepted: "+truncate(m.drainProgress.current, w-24)) + "\n")
	case "evicted", "blocked":
		fmt.Fprintf(&b, " %d / %d terminated\n",
			m.drainProgress.done, m.drainProgress.total)
	case "done", "error":
		fmt.Fprintf(&b, " Drain incomplete: %d / %d terminated\n", m.drainProgress.done, m.drainProgress.total)
		if m.drainProgress.err != "" {
			b.WriteString(" " + truncate(m.drainProgress.err, w-6) + "\n")
		}
	}

	lines := m.drainProgress.blocked
	label := "pod(s) blocked"
	if len(m.drainProgress.remaining) > 0 {
		lines = m.drainProgress.remaining
		label = "pod(s) not confirmed terminated"
	}
	if n := len(lines); n > 0 {
		b.WriteString("\n")
		b.WriteString(m.Theme.StatusWrn.Render(fmt.Sprintf(" %d %s:", n, label)) + "\n")
		// Cap to last 5 — for nodes with dozens of blocked pods the
		// modal would otherwise grow taller than the canvas.
		start := 0
		if n > 5 {
			start = n - 5
			b.WriteString(m.Theme.Dim.Render(fmt.Sprintf("   …%d earlier omitted", start)) + "\n")
		}
		for _, line := range lines[start:] {
			b.WriteString(m.Theme.Dim.Render("   " + truncate(line, w-6)))
			b.WriteByte('\n')
		}
	}

	b.WriteString("\n")
	if m.drainProgress.phase == "done" || m.drainProgress.phase == "error" {
		b.WriteString(m.Theme.Footer.Render(" esc to close  (node remains cordoned)"))
	} else {
		b.WriteString(m.Theme.Footer.Render(" esc to cancel  (accepted evictions continue)"))
	}
	return m.boxed(b.String(), w, canvasWidth, canvasHeight)
}

// boxed wraps `body` in the standard centered modal box and places
// it on the canvas. Kept here local to drain.go so future modal
// renderers in this file (and exec.go) can share, but not promoted
// to render.go until a third caller earns it.
func (m Model) boxed(body string, width, canvasWidth, canvasHeight int) string {
	box := lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("244")).
		Padding(0, 1).
		Width(width).
		Render(body)
	return lipgloss.Place(canvasWidth, canvasHeight, lipgloss.Center, lipgloss.Center, box)
}
