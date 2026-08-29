package ui

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/fmidev/kubetin/internal/cluster"
)

// logsState owns the live logs view: the streaming buffer, scroll
// position, follow flag, and the most recent error if any.
type logsState struct {
	// open means the full-screen viewer is on screen. streaming means
	// a stream is live — the dashboard's log pane renders this same
	// buffer inline with open=false, so the two can't be one flag.
	open       bool
	streaming  bool
	ref        cluster.DescribeRef
	container  string
	containers []string // for the picker; populated when len > 1
	pickerOpen bool
	pickerCur  int

	lines []string // ring buffer; oldest evicted first
	cap   int
	// tail is what this stream requested; full is set once the user
	// has pulled the whole log. Together they drive the "there is more
	// behind this window" hint.
	tail     int
	full     bool
	scroll   int  // 0 = bottom; positive = scrolled up by N rows
	follow   bool // when true, scroll auto-clamps to the bottom
	err      string
	finished bool // EOS observed

	// In-buffer search. Activated by `/`, like the table filter but
	// scoped to the streaming buffer. While searchFocused, key input
	// edits searchTerm; on each edit and on each new line batch we
	// rebuild searchMatches (line indices into lines[]). n/N step
	// through matches, scrolling the viewport so the current match
	// lands inside it.
	searchTerm    string
	searchFocused bool
	searchMatches []int
	searchIdx     int

	// reconnecting is set when the streamer signalled a transient
	// failure and is retrying. Cleared automatically when the next
	// real line arrives. The status indicator shows "↻ reconnecting"
	// while set, instead of the misleading "● live".
	reconnecting bool

	// session is the active log-stream identifier. It's incremented
	// on every startLogs and stamped into LogStartMsg so the
	// forwarder in main can echo it back on every line/EOS/Err. The
	// UI drops messages whose session doesn't match — so a slow
	// forwarder still flushing buffered lines after the user reopens
	// the view (or switches pods) can't contaminate the new stream's
	// state. Same shape as PermissionResultMsg's context-keyed cache.
	session uint64
}

// LogStartMsg asks main to begin streaming. Session is a monotonic
// stream identifier the UI assigns; main echoes it back on every
// downstream message so the UI can drop late lines from a previously-
// cancelled stream.
type LogStartMsg struct {
	Session   uint64
	Ref       cluster.DescribeRef
	Container string
	// Tail is the number of historical lines to replay before
	// following. Negative means the whole log.
	Tail int
}

// LogStopMsg asks main to cancel the active stream (if any).
type LogStopMsg struct{}

// LogLineMsg carries one rendered log line from the streamer. Kept
// for callers that don't batch, but the forwarder in main bundles
// lines into LogLinesMsg to keep the bubbletea event loop quiet under
// chatty pods (kube-system flannel/calico can emit thousands per second).
type LogLineMsg struct {
	Session uint64
	Line    string
}

// LogLinesMsg carries a batch of lines collected over a short window
// in the forwarder. Order preserved.
type LogLinesMsg struct {
	Session uint64
	Lines   []string
}

// LogErrorMsg carries a stream-level error.
type LogErrorMsg struct {
	Session uint64
	Err     string
}

// LogEOSMsg signals the streamer's End-of-Stream.
type LogEOSMsg struct {
	Session uint64
}

// LogReconnectingMsg signals that the streamer hit a transient error
// and is about to retry. Cause is informational (goes to debug.log
// regardless); UI shows a "↻ reconnecting" indicator until the next
// LogLineMsg arrives.
type LogReconnectingMsg struct {
	Session uint64
	Cause   string
}

const (
	// Buffer for a normal tail. 20k lines at a few hundred bytes each
	// is a handful of megabytes, and only one stream runs at a time.
	defaultLogCap = 20000
	// Buffer once the user has asked for the whole log with `L`. They
	// asked, so hold considerably more before evicting.
	fullLogCap    = 200000
	logScrollPage = 10
	logScrollHalf = 5
	// Lines fetched when the viewer opens. 200 showed roughly one
	// screen of history on a chatty pod, which is rarely the window
	// you need when something broke a minute ago. Overridable with
	// -log-tail, and `L` pulls the rest.
	logTailDefault = 2000
	// logTailAll is the sentinel for "no TailLines at all".
	logTailAll      = -1
	logViewMinWidth = 50
)

// startLogs prepares state and returns the LogStartMsg-emitting Cmd.
// Caller has already chosen the container (or there was only one).
// Bumps logs.session so any messages still draining from a previously-
// cancelled stream get filtered out by their stale session id.
func (m Model) startLogs(ref cluster.DescribeRef, container string) (tea.Model, tea.Cmd) {
	cmd := m.beginLogStream(ref, container)
	if cmd == nil {
		return m, nil
	}
	m.logs.open = true
	return m, cmd
}

// beginLogStream resets the buffer and asks main to start streaming,
// without deciding whether anything is shown. startLogs opens the
// full-screen viewer on top of it; the dashboard leaves open=false and
// renders the same buffer in its log pane.
//
// Bumps logs.session so any messages still draining from a previously-
// cancelled stream get filtered out by their stale session id.
func (m *Model) beginLogStream(ref cluster.DescribeRef, container string) tea.Cmd {
	return m.beginLogStreamTail(ref, container, m.logTail())
}

// logTail is the configured initial tail, falling back to the default
// when main hasn't set one.
func (m Model) logTail() int {
	if m.LogTail != 0 {
		return m.LogTail
	}
	return logTailDefault
}

// beginLogStreamTail is beginLogStream with an explicit history depth.
// A negative tail asks for the whole log.
func (m *Model) beginLogStreamTail(ref cluster.DescribeRef, container string, tail int) tea.Cmd {
	if m.OnLogsStart == nil {
		return nil
	}
	m.logs.session++
	m.logs.streaming = true
	m.logs.tail = tail
	m.logs.full = tail < 0
	m.logs.ref = ref
	m.logs.container = container
	m.logs.lines = make([]string, 0, 256)
	m.logs.cap = defaultLogCap
	if tail < 0 {
		m.logs.cap = fullLogCap
	}
	m.logs.scroll = 0
	m.logs.follow = true
	m.logs.err = ""
	m.logs.finished = false
	m.logs.reconnecting = false
	m.logs.searchTerm = ""
	m.logs.searchMatches = nil
	m.logs.searchIdx = 0
	m.logs.searchFocused = false
	cb := m.OnLogsStart
	focused := m.WatchedContext
	req := LogStartMsg{Session: m.logs.session, Ref: ref, Container: container, Tail: tail}
	return func() tea.Msg { return cb(focused, req) }
}

// tailOrDefault reports the tail this stream actually requested,
// falling back for buffers seeded outside beginLogStreamTail (tests,
// and any future caller that fills lines directly).
func (l logsState) tailOrDefault() int {
	if l.tail == 0 {
		return logTailDefault
	}
	return l.tail
}

// logStatusIndicator renders the stream's state badge. Precedence:
// error (and not yet ended) > finished > reconnecting > paused > live.
// "reconnecting" wins over "paused" because the user's paused state
// still applies to the buffer they're reading; the network-level state
// matters more in that moment.
func (m Model) logStatusIndicator() string {
	switch {
	case m.logs.err != "" && !m.logs.finished:
		return m.Theme.StatusBad.Render("✕ " + summariseStreamErr(m.logs.err))
	case m.logs.finished:
		return m.Theme.StatusDim.Render("◼ ended")
	case m.logs.reconnecting:
		return m.Theme.StatusWrn.Render("↻ reconnecting")
	case !m.logs.follow:
		return m.Theme.StatusWrn.Render("❚❚ paused")
	}
	return m.Theme.StatusOK.Render("● live")
}

// openLogsForCursor handles the "Logs" action selection. For a Pod
// row we use the cursor directly; for a Deployment row we pick a
// running pod owned by the deployment from the local cache.
func (m Model) openLogsForCursor(ref cluster.DescribeRef) (tea.Model, tea.Cmd) {
	if ref.Kind == "Deployment" {
		return m.openLogsForDeployment(ref)
	}
	containers := []string{}
	if r, ok := m.pods[m.cursor]; ok {
		containers = r.Containers
	}
	return m.openLogsForPod(ref, containers)
}

// openLogsForPod is the shared entry from both the Pod cursor path
// and the Deployment-pod-resolution path.
func (m Model) openLogsForPod(ref cluster.DescribeRef, containers []string) (tea.Model, tea.Cmd) {
	switch len(containers) {
	case 0:
		// No container info yet (informer hasn't seen it) — try
		// streaming with an empty container; the apiserver picks
		// the only one if there's one, errors if multiple.
		return m.startLogs(ref, "")
	case 1:
		return m.startLogs(ref, containers[0])
	}
	m.logs.open = true
	m.logs.ref = ref
	m.logs.containers = containers
	m.logs.pickerOpen = true
	m.logs.pickerCur = 0
	return m, nil
}

// openLogsForDeployment finds a Running pod from the local cache that
// belongs to the deployment and streams its logs. The owner mapping
// is heuristic: deployment "foo" produces pods "foo-<rs-hash>-<id>",
// so namespace match + Name prefix `foo-` is correct in practice. We
// pick the most recently created Running pod so a recent rollout
// surfaces over older replicas.
func (m Model) openLogsForDeployment(deployRef cluster.DescribeRef) (tea.Model, tea.Cmd) {
	prefix := deployRef.Name + "-"
	var picked *podRow
	for _, p := range m.pods {
		if p.Namespace != deployRef.Namespace {
			continue
		}
		if !strings.HasPrefix(p.Name, prefix) {
			continue
		}
		if string(p.Phase) != "Running" {
			continue
		}
		if picked == nil || p.CreatedAt.After(picked.CreatedAt) {
			pp := p
			picked = &pp
		}
	}
	if picked == nil {
		m.toast = "✕ No Running pod found for " + deployRef.Name
		m.toastUntil = time.Now().Add(3 * time.Second)
		return m, tea.Tick(3*time.Second, func(t time.Time) tea.Msg { return toastClearMsg(t) })
	}
	podRef := cluster.DescribeRef{
		Version: "v1", Resource: "pods", Kind: "Pod",
		Namespace: picked.Namespace, Name: picked.Name,
	}
	return m.openLogsForPod(podRef, picked.Containers)
}

func (m Model) handleLogsKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.logs.pickerOpen {
		return m.handleContainerPickerKey(k)
	}
	if m.logs.searchFocused {
		return m.handleLogsSearchKey(k)
	}
	switch k.String() {
	case "esc", "q":
		// First Esc with an active (but unfocused) search clears it
		// instead of closing the whole view, mirroring the filter UX
		// at the top level.
		if m.logs.searchTerm != "" {
			m.logs.searchTerm = ""
			m.logs.searchMatches = nil
			m.logs.searchIdx = 0
			return m, nil
		}
		return m.closeLogs()
	case "ctrl+c":
		_, cmd := m.closeLogs()
		m.quitMsg = "bye"
		return m, tea.Sequence(cmd, tea.Quit)
	case "/":
		m.logs.searchFocused = true
		// Pause follow while searching — the user wants the viewport
		// stable to read matches, not chasing the tail.
		m.logs.follow = false
	case "n":
		m.stepLogsMatch(+1)
	case "N":
		m.stepLogsMatch(-1)
	case "j", "down":
		if m.logs.scroll > 0 {
			m.logs.scroll--
		}
		if m.logs.scroll == 0 {
			m.logs.follow = true
		}
	case "k", "up":
		m.logs.scroll = clampLogsScroll(m.logs.scroll+1, len(m.logs.lines), m.height)
		m.logs.follow = false
	case "ctrl+d":
		m.logs.scroll -= logScrollPage
		if m.logs.scroll < 0 {
			m.logs.scroll = 0
		}
		if m.logs.scroll == 0 {
			m.logs.follow = true
		}
	case "ctrl+u":
		m.logs.scroll = clampLogsScroll(m.logs.scroll+logScrollPage, len(m.logs.lines), m.height)
		m.logs.follow = false
	case "g", "home":
		// Park the top of the buffer at the top of the viewport, not
		// off the screen — setting scroll = len(lines) would push every
		// line past the upper edge and the viewport would render empty.
		m.logs.scroll = clampLogsScroll(len(m.logs.lines), len(m.logs.lines), m.height)
		m.logs.follow = false
	case "G", "end":
		m.logs.scroll = 0
		m.logs.follow = true
	case "f":
		m.logs.follow = !m.logs.follow
		if m.logs.follow {
			m.logs.scroll = 0
		}
	case "L":
		// Re-request the same pod and container with no tail limit.
		// Costly on a large log, so it's on demand rather than the
		// default — but it's the difference between "the last screen"
		// and "what actually happened".
		if m.logs.full {
			return m, nil
		}
		cmd := m.beginLogStreamTail(m.logs.ref, m.logs.container, logTailAll)
		return m, cmd
	}
	return m, nil
}

// handleLogsSearchKey routes input while the / search prompt is open.
// Live edits rebuild the match list so the count/highlight stay in
// sync as the user types.
func (m Model) handleLogsSearchKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.Type {
	case tea.KeyEsc:
		m.logs.searchTerm = ""
		m.logs.searchMatches = nil
		m.logs.searchIdx = 0
		m.logs.searchFocused = false
		return m, nil
	case tea.KeyEnter:
		// Unfocus but keep the search active so n/N work; jump to the
		// first match if there is one.
		m.logs.searchFocused = false
		m.recomputeLogsMatches()
		if len(m.logs.searchMatches) > 0 {
			m.logs.searchIdx = 0
			m.scrollLogsToMatch(m.logs.searchMatches[0])
		}
		return m, nil
	case tea.KeyBackspace:
		r := []rune(m.logs.searchTerm)
		if len(r) > 0 {
			m.logs.searchTerm = string(r[:len(r)-1])
		}
	case tea.KeySpace:
		m.logs.searchTerm += " "
	case tea.KeyRunes:
		m.logs.searchTerm += string(k.Runes)
	case tea.KeyCtrlC:
		m.quitMsg = "bye"
		return m, tea.Quit
	}
	m.recomputeLogsMatches()
	return m, nil
}

// recomputeLogsMatches walks the buffer for case-insensitive
// substring matches against searchTerm. Empty term clears the list.
func (m *Model) recomputeLogsMatches() {
	if m.logs.searchTerm == "" {
		m.logs.searchMatches = nil
		m.logs.searchIdx = 0
		return
	}
	needle := strings.ToLower(m.logs.searchTerm)
	out := m.logs.searchMatches[:0]
	for i, line := range m.logs.lines {
		if strings.Contains(strings.ToLower(line), needle) {
			out = append(out, i)
		}
	}
	m.logs.searchMatches = out
	if m.logs.searchIdx >= len(out) {
		m.logs.searchIdx = 0
	}
}

// stepLogsMatch moves the search cursor by delta wrap-around through
// the match list and scrolls the viewport so the new match is visible.
func (m *Model) stepLogsMatch(delta int) {
	if len(m.logs.searchMatches) == 0 {
		return
	}
	n := len(m.logs.searchMatches)
	m.logs.searchIdx = ((m.logs.searchIdx+delta)%n + n) % n
	m.scrollLogsToMatch(m.logs.searchMatches[m.logs.searchIdx])
}

// scrollLogsToMatch sets scroll so the given line index is roughly
// centred in the viewport. Approximates bodyHeight from m.height.
func (m *Model) scrollLogsToMatch(lineIdx int) {
	const overhead = 8
	approxBody := m.height - overhead
	if approxBody < 1 {
		approxBody = 1
	}
	// scroll = lines from tail; line lineIdx should sit near
	// the middle of the viewport, so:
	//   visible window = [end - approxBody, end)
	//   centre = end - approxBody/2 ⇒ lineIdx ≈ end - approxBody/2
	//   end = lineIdx + approxBody/2
	//   scroll = len(lines) - end
	want := len(m.logs.lines) - (lineIdx + approxBody/2)
	m.logs.scroll = clampLogsScroll(want, len(m.logs.lines), m.height)
	m.logs.follow = false
}

// clampLogsScroll caps the scroll position so the viewport always
// shows at least some lines. scroll is measured "lines back from the
// tail"; the renderer computes end := len(lines) - scroll and
// start := end - bodyHeight. If scroll exceeds len(lines) - bodyHeight,
// end goes to (or past) the top of the buffer and the viewport
// renders nothing. Cap derives an approximate bodyHeight from total
// terminal height (renderLogs's bodyHeight = canvasHeight - 4 inside
// a box that consumes 4 rows of header+footer+borders from the
// overall canvas, so total overhead ≈ 8 rows).
func clampLogsScroll(want, lineCount, terminalHeight int) int {
	const overhead = 8
	return clampLogsScrollTo(want, lineCount, terminalHeight-overhead)
}

// clampLogsScrollTo bounds a tail-relative offset against an explicit
// viewport height. clampLogsScroll subtracts the full-screen viewer's
// chrome to approximate one; the dashboard's log pane knows its exact
// height from the layout and passes it straight through.
func clampLogsScrollTo(want, lineCount, bodyHeight int) int {
	if want < 0 {
		return 0
	}
	if bodyHeight < 1 {
		bodyHeight = 1
	}
	max := lineCount - bodyHeight
	if max < 0 {
		max = 0
	}
	if want > max {
		return max
	}
	return want
}

// scrollLogsBy moves the shared tail-relative offset by delta rows
// (positive = back into history) against a viewport of bodyHeight, and
// keeps follow in sync: any move off the tail pauses, returning to the
// tail resumes. The dashboard's log pane and the full-screen viewer
// drive the same state, so a pane that's scrolled back stays put as
// new lines arrive and reports itself paused.
func (m *Model) scrollLogsBy(delta, bodyHeight int) {
	m.logs.scroll = clampLogsScrollTo(m.logs.scroll+delta, len(m.logs.lines), bodyHeight)
	m.logs.follow = m.logs.scroll == 0
}

func (m Model) closeLogs() (tea.Model, tea.Cmd) {
	m.logs.open = false
	m.logs.pickerOpen = false
	// The dashboard renders this same buffer in its log pane, so
	// closing the full-screen viewer drops back to it rather than
	// killing the stream out from under it.
	if m.dashboard.open {
		return m, nil
	}
	m.logs.streaming = false
	if m.OnLogsStop != nil {
		cb := m.OnLogsStop
		return m, func() tea.Msg { cb(); return nil }
	}
	return m, nil
}

func (m Model) handleContainerPickerKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc", "q":
		return m.closeLogs()
	case "ctrl+c":
		m.quitMsg = "bye"
		return m, tea.Quit
	case "j", "down":
		if m.logs.pickerCur < len(m.logs.containers)-1 {
			m.logs.pickerCur++
		}
	case "k", "up":
		if m.logs.pickerCur > 0 {
			m.logs.pickerCur--
		}
	case "enter":
		c := m.logs.containers[m.logs.pickerCur]
		m.logs.pickerOpen = false
		return m.startLogs(m.logs.ref, c)
	}
	return m, nil
}

// summariseStreamErr collapses long net/io errors into a short label.
// kubelet's apiserver-proxy log streams routinely time out or get
// reset on idle, producing "read tcp <ip>:<port>->...: connection
// reset by peer" — useful to know once, but not useful to keep on
// screen at full length while the user is trying to search the
// already-buffered lines. Recognised errors get a stable short label;
// anything else is truncated to keep the footer readable.
func summariseStreamErr(err string) string {
	switch {
	case strings.Contains(err, "connection reset by peer"):
		return "connection reset (stream ended)"
	case strings.Contains(err, "i/o timeout"):
		return "stream timed out"
	case strings.Contains(err, "EOF"):
		return "stream closed (EOF)"
	case strings.Contains(err, "context canceled"):
		return "stream cancelled"
	case strings.Contains(err, "no such host"):
		return "DNS lookup failed"
	}
	if len(err) > 60 {
		return err[:57] + "…"
	}
	return err
}

// highlightMatches wraps each occurrence of needle in s with reverse-
// video SGR codes (`\x1b[7m`…`\x1b[27m`). Reverse is toggled rather
// than colour-set, so any ANSI colours embedded in s by the log
// producer stay intact across the highlight — `\x1b[27m` unsets only
// the reverse attribute. lipgloss.Style.Render couldn't be used here
// because it terminates with a full reset (`\x1b[0m`), which kills
// the surrounding log colours.
//
// `bold` makes the highlight bolder (current n/N target) by combining
// reverse + bold SGR codes.
//
// Search is case-insensitive on the line's *visible* text — ANSI
// escape sequences are skipped during matching so a match never falls
// inside an escape and the wrap markers always sit at visible
// boundaries. If needle is empty or has no matches, s is returned
// unchanged.
func highlightMatches(s, needle string, bold bool) string {
	if needle == "" || s == "" {
		return s
	}
	on, off := "\x1b[7m", "\x1b[27m"
	if bold {
		on, off = "\x1b[1;7m", "\x1b[27;22m"
	}

	// Build a parallel "visible byte index → raw byte index" map by
	// walking s and skipping CSI escape sequences.
	visToRaw := make([]int, 0, len(s))
	visible := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			// CSI: ESC [ ... <final>. Final is in 0x40..0x7E.
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7E) {
				j++
			}
			if j < len(s) {
				j++
			}
			i = j
			continue
		}
		visToRaw = append(visToRaw, i)
		visible = append(visible, s[i])
		i++
	}

	visStr := string(visible)
	visLower := strings.ToLower(visStr)
	needleLower := strings.ToLower(needle)
	if needleLower == "" {
		return s
	}

	type span struct{ rawStart, rawEnd int }
	var spans []span
	for off2 := 0; off2 < len(visLower); {
		idx := strings.Index(visLower[off2:], needleLower)
		if idx < 0 {
			break
		}
		visStart := off2 + idx
		visEnd := visStart + len(needleLower)
		rawStart := visToRaw[visStart]
		var rawEnd int
		if visEnd < len(visToRaw) {
			rawEnd = visToRaw[visEnd]
		} else {
			rawEnd = len(s)
		}
		spans = append(spans, span{rawStart, rawEnd})
		off2 = visEnd
	}
	if len(spans) == 0 {
		return s
	}

	var b strings.Builder
	b.Grow(len(s) + len(spans)*(len(on)+len(off)))
	cursor := 0
	for _, sp := range spans {
		b.WriteString(s[cursor:sp.rawStart])
		b.WriteString(on)
		b.WriteString(s[sp.rawStart:sp.rawEnd])
		b.WriteString(off)
		cursor = sp.rawEnd
	}
	b.WriteString(s[cursor:])
	return b.String()
}

// sanitizeLogLine reduces a raw log line to text plus SGR colour
// sequences. Everything else a container can write to its stdout —
// cursor moves, screen erases, OSC title sets, charset switches, bare
// control bytes — is dropped instead of being forwarded to the
// terminal, where it would move the cursor outside the box we drew or
// leave a mode set that survives closing the viewer.
//
// Sanitizing at ingest rather than at render is deliberate: the ring
// buffer is also what search reads and what the dashboard's splice
// helpers walk, and those assume every escape in a line is `CSI … m`.
func sanitizeLogLine(s string) string {
	if !needsSanitizing(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == 0x1b:
			i = copySGR(&b, s, i)
			continue
		case c == '\t':
			// A raw tab jumps to the terminal's own tab stops, which
			// the box we render into doesn't share, and measures as
			// zero cells so the padding maths under-counts it too.
			b.WriteString("    ")
			i++
			continue
		case c < 0x20 || c == 0x7f:
			i++
			continue
		case c < utf8.RuneSelf:
			b.WriteByte(c)
			i++
			continue
		}
		// Non-ASCII. Decode rather than copy bytes: a lone 0x9b is
		// CSI to a terminal that hasn't been switched into UTF-8
		// mode, and it reaches us intact because Scanner.Text() does
		// no validation. Drop the C1 range in both its raw and its
		// UTF-8 form, and drop bytes that decode to nothing at all.
		r, size := utf8.DecodeRuneInString(s[i:])
		if (r == utf8.RuneError && size == 1) || r <= 0x9f {
			i += size
			continue
		}
		b.WriteString(s[i : i+size])
		i += size
	}
	return b.String()
}

// needsSanitizing is sanitizeLogLine's fast path. A line of printable
// text — ASCII or well-formed multibyte — keeps its backing string
// with no copy.
func needsSanitizing(s string) bool {
	for i := 0; i < len(s); {
		c := s[i]
		if c < 0x20 || c == 0x7f {
			return true
		}
		if c < utf8.RuneSelf {
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if (r == utf8.RuneError && size == 1) || r <= 0x9f {
			return true
		}
		i += size
	}
	return false
}

// copySGR consumes the escape sequence starting at s[i] (which is
// ESC), writing it through only when it is an SGR sequence — `CSI … m`,
// the colours the viewer is built to preserve. Returns the index one
// past the sequence.
func copySGR(b *strings.Builder, s string, i int) int {
	if i+1 >= len(s) {
		return len(s)
	}
	switch s[i+1] {
	case '[':
		// CSI: parameter and intermediate bytes (0x20–0x3f) followed
		// by one final byte (0x40–0x7e).
		j := i + 2
		sgr := true
		for j < len(s) && s[j] >= 0x20 && s[j] <= 0x3f {
			// A final 'm' alone doesn't make it a colour: SGR takes
			// digits, ';' and ':' (true-colour subparameters) and
			// nothing else. `CSI > 4;2 m` sets xterm's modifyOtherKeys
			// — a terminal mode that changes what keys the app sees,
			// which no reset undoes and which outlives the viewer.
			if (s[j] < '0' || s[j] > '9') && s[j] != ';' && s[j] != ':' {
				sgr = false
			}
			j++
		}
		if j >= len(s) || s[j] < 0x40 || s[j] > 0x7e {
			return j // line ended mid-sequence
		}
		if sgr && s[j] == 'm' {
			b.WriteString(s[i : j+1])
		}
		return j + 1
	case ']', 'P', 'X', '^', '_':
		// OSC/DCS/SOS/PM/APC carry a payload up to BEL or ST. ST has
		// three spellings and a terminal honours all of them, so we
		// have to as well — stopping only at `ESC \` would swallow
		// the ordinary log text after a C1-terminated one.
		for j := i + 2; j < len(s); j++ {
			switch {
			case s[j] == 0x07 || s[j] == 0x9c:
				return j + 1
			case s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\':
				return j + 2
			case s[j] == 0xc2 && j+1 < len(s) && s[j+1] == 0x9c:
				return j + 2
			}
		}
		return len(s)
	}
	// Everything else: optional intermediates, then one final byte.
	j := i + 1
	for j < len(s) && s[j] >= 0x20 && s[j] <= 0x2f {
		j++
	}
	if j < len(s) {
		j++
	}
	return j
}

// fitLogLine truncates a sanitized log line to width visible cells and
// terminates any colour it leaves open.
//
// Both halves matter. `truncate` measures escape bytes as visible
// cells and slices at a byte offset, so a coloured line came out cut
// mid-sequence (`ESC [ 3` + the ellipsis) *and* stripped of its
// closing `ESC [ 39 m` — the terminal then ate the following bytes
// looking for a final byte, and the colour bled down the rest of the
// screen and outlived the viewer. visiblePrefix cuts on visible-cell
// boundaries and never splits an escape; the trailing reset covers
// both the codes the cut dropped and producers that never reset at
// all.
func fitLogLine(s string, width int) string {
	if width <= 0 {
		return ""
	}
	out := s
	if lipgloss.Width(s) > width {
		prefix, _ := visiblePrefix(s, width-1)
		out = prefix + "…"
	}
	if strings.IndexByte(out, 0x1b) >= 0 {
		out += "\x1b[0m"
	}
	return out
}

// applyLogLine appends a line to the ring buffer with capacity bound.
// While the user is paused (!follow), bump scroll by the number of
// new lines so the visible window stays anchored to the same absolute
// content — without this the viewport drifts back toward the tail
// because scroll is measured from len(lines), which just grew.
func (m *Model) applyLogLine(line string) {
	m.applyLogLines([]string{line})
}

// applyLogLines is the batch counterpart — same trimming, called once
// per batch instead of once per line. Owns the pause-anchoring logic
// so per-line and batched paths can't diverge.
func (m *Model) applyLogLines(lines []string) {
	if len(lines) == 0 {
		return
	}
	for _, ln := range lines {
		m.logs.lines = append(m.logs.lines, sanitizeLogLine(ln))
	}
	added := len(lines)

	// Trim the head if we're over cap. A trim of N means the absolute
	// position the user is pinned to has slid N indices to the left,
	// so the offset-from-tail decreases by N (clamped at 0).
	dropped := 0
	if len(m.logs.lines) > m.logs.cap {
		dropped = len(m.logs.lines) - m.logs.cap
		m.logs.lines = m.logs.lines[dropped:]
	}

	if !m.logs.follow {
		// Net effect on a tail-relative scroll counter: tail advances
		// by `added`, so we need +added to stay in place. Cap-trim
		// shifts the absolute origin by `dropped` lines that have
		// fallen off the bottom of history; that subtracts from how
		// far back we are. Then clamp so we never point past either end.
		m.logs.scroll += added - dropped
		if m.logs.scroll < 0 {
			m.logs.scroll = 0
		}
		if m.logs.scroll > len(m.logs.lines) {
			m.logs.scroll = len(m.logs.lines)
		}
	}

	// Keep search results in sync. Indices in searchMatches point into
	// lines[], so a head-trim invalidates the lower indices entirely
	// and shifts the rest. Cheaper to just recompute than to walk and
	// rebase — the buffer is bounded by m.logs.cap and the term is a
	// substring scan, not a regex.
	if m.logs.searchTerm != "" {
		m.recomputeLogsMatches()
	}
}

// renderLogs draws the logs viewport.
func (m Model) renderLogs(canvasWidth, canvasHeight int) string {
	if m.logs.pickerOpen {
		return m.renderContainerPicker(canvasWidth, canvasHeight)
	}

	w := canvasWidth
	if w < logViewMinWidth {
		w = logViewMinWidth
	}
	h := canvasHeight
	if h < 6 {
		h = 6
	}

	// Inner content width inside the bordered box is `Width(w-2)` minus
	// the two border columns → w-4. Anything wider wraps and silently
	// adds a row, which busts the canvasHeight budget downstream.
	innerW := w - 4
	if innerW < 1 {
		innerW = 1
	}

	var b strings.Builder
	title := m.Theme.Title.Render(" logs ")
	subject := m.Theme.Dim.Render(fmt.Sprintf(" %s · %s/%s · %s",
		m.WatchedContext, m.logs.ref.Namespace, m.logs.ref.Name, m.logs.container))
	b.WriteString(title + subject + "\n")
	b.WriteString(m.Theme.Dim.Render(strings.Repeat("─", innerW)) + "\n")

	bodyHeight := h - 4 // title, sep, footer, blank
	if bodyHeight < 1 {
		bodyHeight = 1
	}

	lines := m.logs.lines
	end := len(lines) - m.logs.scroll
	if end < 0 {
		end = 0
	}
	start := end - bodyHeight
	if start < 0 {
		start = 0
	}

	// Build a quick lookup so per-line rendering can highlight matches
	// without walking searchMatches each iteration. currentMatchLine
	// is the absolute line index of the active n/N target — that line
	// gets a brighter highlight than other matches.
	var matchSet map[int]struct{}
	currentMatchLine := -1
	if len(m.logs.searchMatches) > 0 {
		matchSet = make(map[int]struct{}, len(m.logs.searchMatches))
		for _, idx := range m.logs.searchMatches {
			matchSet[idx] = struct{}{}
		}
		if m.logs.searchIdx < len(m.logs.searchMatches) {
			currentMatchLine = m.logs.searchMatches[m.logs.searchIdx]
		}
	}

	for i, ln := range lines[start:end] {
		absIdx := start + i
		rendered := fitLogLine(ln, innerW)
		if matchSet != nil {
			if _, hit := matchSet[absIdx]; hit {
				bold := absIdx == currentMatchLine
				rendered = highlightMatches(rendered, m.logs.searchTerm, bold)
			}
		}
		b.WriteString(rendered + "\n")
	}
	// Pad to keep the footer at the bottom of the viewport.
	for i := end - start; i < bodyHeight; i++ {
		b.WriteByte('\n')
	}

	status := m.logStatusIndicator()

	// Search prompt: when focused, show "/term█"; when active but
	// unfocused, show "/term  M/N" plus the n/N hint.
	searchInfo := ""
	switch {
	case m.logs.searchFocused:
		caret := m.Theme.Title.Render("█")
		searchInfo = "  " + m.Theme.Title.Render("/") + m.logs.searchTerm + caret
	case m.logs.searchTerm != "":
		count := fmt.Sprintf("%d/%d", m.logs.searchIdx+1, len(m.logs.searchMatches))
		if len(m.logs.searchMatches) == 0 {
			count = "no match"
		}
		searchInfo = "  " + m.Theme.Title.Render("/") + m.logs.searchTerm +
			m.Theme.Dim.Render("  "+count)
	}

	hint := " · /:search"
	if m.logs.searchTerm != "" {
		hint = " · n/N:next/prev  /:edit"
	}
	// Say whether this is the whole log or a window onto the tail of
	// one — otherwise "1200 lines" reads as the complete story when
	// it's the last 2000 of a million.
	scope := "full log"
	if !m.logs.full {
		scope = fmt.Sprintf("tail %d · L:all", m.logs.tailOrDefault())
	}
	rest := m.Theme.Footer.Render(fmt.Sprintf(
		" · %d lines (%s)%s  ·  j/k scroll  G follow  f toggle  Esc close",
		len(m.logs.lines), scope, hint))
	b.WriteString(" " + status + searchInfo + rest)

	box := lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("244")).
		Width(w - 2).
		Render(b.String())

	return lipgloss.Place(canvasWidth, canvasHeight, lipgloss.Center, lipgloss.Center, box)
}

func (m Model) renderContainerPicker(canvasWidth, canvasHeight int) string {
	const w = 42
	var b strings.Builder
	b.WriteString(m.Theme.Title.Render(" container ") +
		m.Theme.Dim.Render(" "+m.logs.ref.Kind+"/"+m.logs.ref.Name) + "\n")
	b.WriteString(m.Theme.Dim.Render(strings.Repeat("─", w-2)) + "\n")
	for i, c := range m.logs.containers {
		marker := "  "
		if i == m.logs.pickerCur {
			marker = m.Theme.Title.Render(" ›")
		}
		line := fmt.Sprintf("%s %s", marker, c)
		if i == m.logs.pickerCur {
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

	return lipgloss.Place(canvasWidth, canvasHeight, lipgloss.Center, lipgloss.Center, box)
}

// openLogsForCursorKey is the `l` shortcut: logs for the highlighted
// row, the same one-key shape `e` gives events and `i` gives the
// dashboard. Previously logs were only reachable through the action
// menu.
//
// Only pods and deployments have logs. Anything else says so rather
// than falling through to openLogsForPod, which would stream the
// cursor's containers against a ref of the wrong kind.
func (m Model) openLogsForCursorKey() (tea.Model, tea.Cmd) {
	ref, ok := m.refForCursor()
	if !ok {
		return m, nil
	}
	switch ref.Kind {
	case "Pod", "Deployment":
		// Honour a cached denial the way the action menu (which hides
		// the item) and the dashboard (which renders the pane as
		// denied) already do. Without this, `l` is a way around the
		// RBAC gate that opens a viewer only to fill it with a 403.
		//
		// Only a *cached* denial refuses. An unknown permission stays
		// optimistic: that request is what populates the cache.
		key := cluster.PermissionKey(m.WatchedContext, "get", "", "pods/log", ref.Namespace)
		if st, ok := m.permissions[key]; ok && !st.Allowed {
			return m.logsDeniedToast(st.Reason)
		}
		return m.openLogsForCursor(ref)
	}
	m.toast = "✕ No logs for " + ref.Kind + "/" + ref.Name
	m.toastUntil = time.Now().Add(3 * time.Second)
	return m, tea.Tick(3*time.Second, func(t time.Time) tea.Msg { return toastClearMsg(t) })
}

// logsDeniedToast reports an RBAC refusal in the footer rather than
// opening a viewer that can only show the error.
func (m Model) logsDeniedToast(reason string) (tea.Model, tea.Cmd) {
	msg := "✕ Not allowed to read pod logs (get pods/log)"
	if reason != "" {
		msg += ": " + reason
	}
	m.toast = msg
	m.toastUntil = time.Now().Add(4 * time.Second)
	return m, tea.Tick(4*time.Second, func(t time.Time) tea.Msg { return toastClearMsg(t) })
}
