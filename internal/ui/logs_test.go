package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

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
