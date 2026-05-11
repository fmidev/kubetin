package ui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/fmidev/kubetin/internal/cluster"
)

// drainConfirmState is the simple y/N modal that gates Drain. We
// don't ask the user to type the node name (the way the Delete
// confirm does) because drain is recoverable — uncordon brings the
// node back. Delete is irreversible; drain is just expensive.
type drainConfirmState struct {
	open    bool
	node    string
	pending bool // true while waiting for the drain start handshake
}

// drainProgressState backs the live progress modal that opens once
// the drain is dispatched. It absorbs the stream of cluster.DrainProgress
// events the supervisor sends down a channel via DrainProgressMsg.
//
// "blocked" pods (PDB violations past retries) accumulate in a list
// so the user can see exactly what's stuck rather than just a count;
// successfully-evicted pods just bump the Done counter.
type drainProgressState struct {
	open    bool
	context string // origin cluster, for the Tab-away guard
	node    string
	current string // pod currently being evicted
	done    int
	total   int
	blocked []string // ns/name of pods that exceeded PDB retries
	phase   string   // mirrors cluster.DrainProgress.Phase
	err     string
	cancel  func()
	started time.Time
}

// DrainStartMsg / DrainProgressMsg / DrainDoneMsg are the lifecycle
// of one drain operation. Start surfaces a fatal setup error (RBAC
// or kubeconfig failure before the first evict); Progress is one
// event per pod / phase transition; Done fires once and closes the
// modal.
type DrainStartMsg struct {
	Context string
	Node    string
	Err     string // non-empty means we never started the eviction loop
	Cancel  func()
}

type DrainProgressMsg cluster.DrainProgress

type DrainDoneMsg struct {
	Context string
	Node    string
	Done    int
	Total   int
	Err     string
	Blocked []string
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
		m.toast = "✕ Drain: " + msg.Err
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
	case "evicting":
		m.drainProgress.current = msg.Pod
	case "blocked":
		m.drainProgress.blocked = append(m.drainProgress.blocked,
			msg.Pod+" ("+msg.Err+")")
	}
	return m, nil
}

// applyDrainDone is the terminal event. We close the progress modal
// and surface a toast with the summary.
func (m Model) applyDrainDone(msg DrainDoneMsg) (tea.Model, tea.Cmd) {
	if msg.Context != m.WatchedContext {
		return m, nil
	}
	m.drainProgress.open = false
	m.drainProgress.cancel = nil
	if msg.Err != "" {
		m.toast = fmt.Sprintf("✕ Drain %s: %s", msg.Node, msg.Err)
	} else if len(msg.Blocked) > 0 {
		m.toast = fmt.Sprintf("⚠ Drained %s: %d/%d (%d blocked by PDB)",
			msg.Node, msg.Done, msg.Total, len(msg.Blocked))
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
	b.WriteString(" This will cordon the node and evict every pod on it\n")
	b.WriteString(" (except mirror pods, DaemonSet-owned pods, and pods\n")
	b.WriteString(" that are already Succeeded / Failed). Pods blocked\n")
	b.WriteString(" by a PodDisruptionBudget are reported individually.\n\n")
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
		b.WriteString(" cordoning + listing pods…\n")
	case "evicting":
		fmt.Fprintf(&b, " %d / %d evicted    %s\n",
			m.drainProgress.done, m.drainProgress.total,
			m.Theme.Dim.Render("evicting "+m.drainProgress.current))
	case "evicted", "blocked":
		fmt.Fprintf(&b, " %d / %d evicted\n",
			m.drainProgress.done, m.drainProgress.total)
	}

	if n := len(m.drainProgress.blocked); n > 0 {
		b.WriteString("\n")
		b.WriteString(m.Theme.StatusWrn.Render(fmt.Sprintf(" %d pod(s) blocked by PDB:\n", n)))
		// Cap to last 5 — for nodes with dozens of blocked pods the
		// modal would otherwise grow taller than the canvas.
		start := 0
		if n > 5 {
			start = n - 5
			b.WriteString(m.Theme.Dim.Render(fmt.Sprintf("   …%d earlier omitted\n", start)))
		}
		for _, line := range m.drainProgress.blocked[start:] {
			b.WriteString(m.Theme.Dim.Render("   " + truncate(line, w-6)))
			b.WriteByte('\n')
		}
	}

	b.WriteString("\n")
	b.WriteString(m.Theme.Footer.Render(" esc to cancel  (already-evicted pods stay evicted)"))
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
