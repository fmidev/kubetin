package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/fmidev/kubetin/internal/cluster"
	"github.com/fmidev/kubetin/internal/model"
)

// highlightMatches must wrap exactly the matched span and leave any
// pre-existing ANSI escape codes in the surrounding text intact.
// Reverse-on/off (`\x1b[7m`/`\x1b[27m`) is the only attribute we
// touch; the source line's foreground/background colours must keep
// rendering across the highlight boundary.
func TestHighlightMatches_PreservesEmbeddedANSI(t *testing.T) {
	// Log line with a coloured "world" — `\x1b[31m` red, `\x1b[0m` reset.
	line := "hello \x1b[31mworld\x1b[0m of clusters"
	out := highlightMatches(line, "world", false)

	// The red code before the match must still be present *before*
	// the highlight on; otherwise the keyword loses the line's colour.
	if !strings.Contains(out, "\x1b[31m") {
		t.Errorf("output dropped the line's red colour: %q", out)
	}
	// The match must be wrapped with reverse-on / reverse-off, not a
	// full reset (`\x1b[0m`). A full reset would terminate the red.
	if !strings.Contains(out, "\x1b[7m") {
		t.Errorf("output missing reverse-on: %q", out)
	}
	if !strings.Contains(out, "\x1b[27m") {
		t.Errorf("output missing reverse-off: %q", out)
	}
	// The keyword itself must appear inside the reverse pair.
	idxOn := strings.Index(out, "\x1b[7m")
	idxOff := strings.Index(out, "\x1b[27m")
	if idxOn < 0 || idxOff < 0 || idxOn >= idxOff {
		t.Fatalf("reverse markers not in order: %q", out)
	}
	span := out[idxOn:idxOff]
	if !strings.Contains(span, "world") {
		t.Errorf("highlighted span doesn't contain the keyword: %q", span)
	}
}

// Case-insensitive match: searching "ERROR" must find "Error" and
// "error" alike.
func TestHighlightMatches_CaseInsensitive(t *testing.T) {
	line := "Error: connection lost. error count = 3"
	out := highlightMatches(line, "ERROR", false)
	count := strings.Count(out, "\x1b[7m")
	if count != 2 {
		t.Errorf("expected 2 reverse-on markers, got %d in %q", count, out)
	}
}

// Empty needle or no match returns the original string unchanged.
func TestHighlightMatches_NoMatch(t *testing.T) {
	cases := []struct {
		name, line, needle string
	}{
		{"empty needle", "hello world", ""},
		{"no match", "hello world", "xyzzy"},
		{"empty line", "", "anything"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if out := highlightMatches(tc.line, tc.needle, false); out != tc.line {
				t.Errorf("expected unchanged %q, got %q", tc.line, out)
			}
		})
	}
}

// Bold variant uses `\x1b[1;7m` so the current n/N target stands out
// from other matches.
func TestHighlightMatches_BoldVariant(t *testing.T) {
	out := highlightMatches("foo bar baz", "bar", true)
	if !strings.Contains(out, "\x1b[1;7m") {
		t.Errorf("bold variant missing combined SGR: %q", out)
	}
	if !strings.Contains(out, "\x1b[27;22m") {
		t.Errorf("bold variant missing combined unset SGR: %q", out)
	}
}

// A match that falls between two ANSI escape sequences must still be
// found at the visible-text level. The escape itself must not count
// as part of the visible string.
func TestHighlightMatches_MatchAcrossANSI(t *testing.T) {
	// "match" split by an irrelevant escape inside it.
	line := "ma\x1b[1mtch ends"
	out := highlightMatches(line, "match", false)
	// Should still wrap something — the original "ma" + escape + "tch"
	// is "match" at visible level.
	if !strings.Contains(out, "\x1b[7m") || !strings.Contains(out, "\x1b[27m") {
		t.Errorf("expected highlight wrapping a split match: %q", out)
	}
}

// summariseStreamErr collapses common kubelet/apiserver stream errors
// to short labels and length-caps unfamiliar ones.
func TestSummariseStreamErr(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"connection reset", "read tcp 192.168.1.52:58707->192.168.1.40:16443: read: connection reset by peer", "connection reset (stream ended)"},
		{"i/o timeout", "read tcp ...: i/o timeout", "stream timed out"},
		{"EOF", "stream error EOF here", "stream closed (EOF)"},
		{"context canceled", "context canceled", "stream cancelled"},
		{"DNS", "dial tcp: lookup foo: no such host", "DNS lookup failed"},
		{"short unknown", "boom", "boom"},
		{"long unknown truncated", strings.Repeat("a", 100), strings.Repeat("a", 57) + "…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := summariseStreamErr(tc.in); got != tc.want {
				t.Errorf("summariseStreamErr(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// 200 lines was roughly one screen of history on a chatty pod, which
// is rarely the window you need when something broke a minute ago.
func TestLogTailDefaultIsRequested(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha"})
	m.width, m.height = 120, 24

	var got LogStartMsg
	m.OnLogsStart = func(_ string, req LogStartMsg) tea.Msg { got = req; return nil }

	out, cmd := m.startLogs(cluster.DescribeRef{
		Version: "v1", Resource: "pods", Kind: "Pod",
		Namespace: "default", Name: "api",
	}, "app")
	if cmd != nil {
		cmd()
	}
	if got.Tail != logTailDefault {
		t.Errorf("requested tail %d, want %d", got.Tail, logTailDefault)
	}
	o := out.(Model)
	if o.logs.cap != defaultLogCap {
		t.Errorf("cap = %d, want %d — a tail bigger than the buffer is discarded", o.logs.cap, defaultLogCap)
	}
	if o.logs.cap < got.Tail {
		t.Errorf("buffer (%d) smaller than the tail requested (%d)", o.logs.cap, got.Tail)
	}
}

// -log-tail overrides the default, including asking for everything.
func TestLogTailFlagOverridesDefault(t *testing.T) {
	for _, want := range []int{50, 100000, logTailAll} {
		m := New("alpha", model.NewStore(), []string{"alpha"})
		m.width, m.height = 120, 24
		m.LogTail = want

		var got LogStartMsg
		m.OnLogsStart = func(_ string, req LogStartMsg) tea.Msg { got = req; return nil }
		_, cmd := m.startLogs(cluster.DescribeRef{Kind: "Pod", Namespace: "default", Name: "api"}, "app")
		if cmd != nil {
			cmd()
		}
		if got.Tail != want {
			t.Errorf("LogTail=%d requested %d", want, got.Tail)
		}
	}
}

// L re-requests the same pod and container with no tail limit, and
// raises the buffer to match — otherwise the extra history is fetched
// only to be evicted.
func TestLogsLoadAllKey(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha"})
	m.width, m.height = 120, 24

	var got LogStartMsg
	m.OnLogsStart = func(_ string, req LogStartMsg) tea.Msg { got = req; return nil }

	opened, cmd := m.startLogs(cluster.DescribeRef{
		Version: "v1", Resource: "pods", Kind: "Pod",
		Namespace: "default", Name: "api",
	}, "envoy")
	if cmd != nil {
		cmd()
	}
	o := opened.(Model)
	firstSession := o.logs.session

	full, cmd := o.handleLogsKey(key("L"))
	if cmd != nil {
		cmd()
	}
	f := full.(Model)

	if got.Tail != logTailAll {
		t.Errorf("L requested tail %d, want %d", got.Tail, logTailAll)
	}
	if got.Ref.Name != "api" || got.Container != "envoy" {
		t.Errorf("L reloaded %s/%s, want the same pod and container", got.Ref.Name, got.Container)
	}
	if got.Session == firstSession {
		t.Error("L reused the session id; late lines from the old stream would contaminate the new one")
	}
	if !f.logs.full {
		t.Error("logs.full not set after L")
	}
	if f.logs.cap != fullLogCap {
		t.Errorf("cap = %d after L, want %d", f.logs.cap, fullLogCap)
	}
}

// Pressing L twice must not restart a stream that already has
// everything.
func TestLogsLoadAllIsIdempotent(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha"})
	m.width, m.height = 120, 24
	calls := 0
	m.OnLogsStart = func(string, LogStartMsg) tea.Msg { calls++; return nil }

	o, cmd := m.startLogs(cluster.DescribeRef{Kind: "Pod", Namespace: "default", Name: "api"}, "app")
	if cmd != nil {
		cmd()
	}
	full, cmd := o.(Model).handleLogsKey(key("L"))
	if cmd != nil {
		cmd()
	}
	again, cmd := full.(Model).handleLogsKey(key("L"))
	if cmd != nil {
		cmd()
	}
	if calls != 2 {
		t.Errorf("OnLogsStart called %d times, want 2 (open + one L)", calls)
	}
	if !again.(Model).logs.full {
		t.Error("second L cleared the full flag")
	}
}

// The footer has to distinguish a window onto the tail from the whole
// log — "1200 lines" reads as the complete story otherwise.
func TestLogsFooterReportsScope(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha"})
	m.width, m.height = 120, 30
	m.OnLogsStart = func(string, LogStartMsg) tea.Msg { return nil }

	o, _ := m.startLogs(cluster.DescribeRef{Kind: "Pod", Namespace: "default", Name: "api"}, "app")
	tailed := o.(Model)
	tailed.logs.lines = []string{"a", "b"}
	out := tailed.renderLogs(120, 20)
	if !strings.Contains(out, "L:all") {
		t.Errorf("tailed viewer should offer L:all:\n%s", out)
	}

	full, _ := tailed.handleLogsKey(key("L"))
	f := full.(Model)
	f.logs.lines = []string{"a", "b"}
	out = f.renderLogs(120, 20)
	if !strings.Contains(out, "full log") {
		t.Errorf("full viewer should say so:\n%s", out)
	}
	if strings.Contains(out, "L:all") {
		t.Error("full viewer still advertises L:all")
	}
}

// sanitizeLogLine must keep colours and drop everything else a
// container can write to its stdout. The failure it guards against is
// a pod whose output moves the cursor, clears the screen, or sets the
// terminal title — all of which land outside the box we drew and
// outlive the viewer.
func TestSanitizeLogLine(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "2026-08-29T21:29:59Z started", "2026-08-29T21:29:59Z started"},
		{"sgr kept", "a \x1b[31mred\x1b[0m b", "a \x1b[31mred\x1b[0m b"},
		{"cursor move dropped", "a\x1b[5Ab", "ab"},
		{"erase display dropped", "a\x1b[2Jb", "ab"},
		{"scroll region dropped", "a\x1b[1;10rb", "ab"},
		{"osc title dropped", "a\x1b]0;pwn\x07b", "ab"},
		{"osc st-terminated dropped", "a\x1b]0;pwn\x1b\\b", "ab"},
		// ST has three spellings; a terminal honours all of them, so
		// the text after one must survive rather than being eaten as
		// part of the control string.
		{"osc c1-st-terminated dropped", "a\x1b]0;pwn\x9crest", "arest"},
		{"osc utf8-st-terminated dropped", "a\x1b]0;pwn\u009crest", "arest"},
		{"charset switch dropped", "a\x1b(0b", "ab"},
		{"carriage return dropped", "50%\r100%", "50%100%"},
		{"backspace dropped", "ab\x08c", "abc"},
		{"del dropped", "a\x7fb", "ab"},
		{"tab expanded", "a\tb", "a    b"},
		{"c1 csi dropped", "a\u009b31mb", "a31mb"},
		// A raw 0x9b is CSI to a terminal that is not in UTF-8 mode,
		// and it reaches us intact — Scanner.Text() validates nothing.
		// Dropping the introducer is what defuses it; its parameters
		// are only text once it is gone.
		{"raw c1 introducer dropped", "a\x9b2Jb", "a2Jb"},
		{"invalid utf8 dropped", "a\xffb", "ab"},
		{"valid multibyte kept", "käännös 日本語 ✓", "käännös 日本語 ✓"},
		{"private csi dropped", "a\x1b[>4;2mb", "ab"},
		{"intermediate csi dropped", "a\x1b[ qb", "ab"},
		{"true-colour subparams kept", "a\x1b[38:2::255:0:0mred\x1b[0m", "a\x1b[38:2::255:0:0mred\x1b[0m"},
		{"trailing esc dropped", "abc\x1b", "abc"},
		{"unterminated csi dropped", "abc\x1b[31", "abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeLogLine(tc.in); got != tc.want {
				t.Errorf("sanitizeLogLine(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Chatty pods push thousands of lines a second through here, so a
// line with nothing to strip must not cost a copy.
func TestSanitizeLogLinePlainPathDoesNotAllocate(t *testing.T) {
	for _, in := range []string{
		"2026-08-29T21:29:59Z nothing interesting here",
		"2026-08-29T21:29:59Z käännös epäonnistui 日本語",
	} {
		if n := testing.AllocsPerRun(100, func() { sanitizeLogLine(in) }); n != 0 {
			t.Errorf("%q allocated %.0f times", in, n)
		}
	}
}

// The bug this fixes: `truncate` counted escape bytes as visible cells
// and cut at a byte offset, so a coloured line lost its closing
// `\x1b[39m` — the colour then bled over the rest of the UI and
// survived closing the viewer — and could be cut mid-sequence
// (`\x1b[3` + "…", which the terminal keeps consuming). fitLogLine
// cuts on cell boundaries and always closes what the line opened.
func TestFitLogLineNeverLeaksColour(t *testing.T) {
	// Shape taken from a real smartmetserver line: bg, bold, fg, then
	// the matching unsets at the end.
	line := "2026-08-29T21:29:59Z \x1b[42m\x1b[1m\x1b[37mLaunched Synapse server\x1b[39m\x1b[22m\x1b[49m"

	for w := 1; w <= 80; w++ {
		out := fitLogLine(line, w)
		if got := lipgloss.Width(out); got > w {
			t.Fatalf("w=%d: rendered %d cells: %q", w, got, out)
		}
		// Strip every complete SGR sequence; an ESC left in what
		// remains is a sequence the cut sliced through.
		var stripped strings.Builder
		for i := 0; i < len(out); {
			if end, ok := sgrEnd(out, i); ok {
				i = end
				continue
			}
			stripped.WriteByte(out[i])
			i++
		}
		if strings.ContainsRune(stripped.String(), 0x1b) {
			t.Fatalf("w=%d: partial escape survived: %q", w, out)
		}
		if strings.ContainsRune(out, 0x1b) && !strings.HasSuffix(out, "\x1b[0m") {
			t.Fatalf("w=%d: colour left open: %q", w, out)
		}
	}
}

// sgrEnd reports the index just past a complete `CSI … m` at s[i].
func sgrEnd(s string, i int) (int, bool) {
	if i+1 >= len(s) || s[i] != 0x1b || s[i+1] != '[' {
		return i, false
	}
	j := i + 2
	for j < len(s) && s[j] >= '0' && s[j] <= ';' {
		j++
	}
	if j < len(s) && s[j] == 'm' {
		return j + 1, true
	}
	return i, false
}

// An uncoloured line must not pick up a reset it doesn't need.
func TestFitLogLineLeavesPlainLinesAlone(t *testing.T) {
	if got := fitLogLine("plain line", 40); got != "plain line" {
		t.Errorf("got %q", got)
	}
	if got := fitLogLine("plain line", 6); got != "plain…" {
		t.Errorf("got %q", got)
	}
}

// Sanitizing happens on ingest so search, the dashboard pane's splice
// helpers, and both renderers all see one clean representation.
func TestApplyLogLinesSanitizes(t *testing.T) {
	m := New("alpha", model.NewStore(), []string{"alpha"})
	m.logs.cap = 100
	m.applyLogLines([]string{"boom\x1b[2J\x1b[H", "\x1b[31mred\x1b[0m"})

	if got := m.logs.lines[0]; got != "boom" {
		t.Errorf("erase/home not stripped: %q", got)
	}
	if got := m.logs.lines[1]; got != "\x1b[31mred\x1b[0m" {
		t.Errorf("colour not preserved: %q", got)
	}
}
