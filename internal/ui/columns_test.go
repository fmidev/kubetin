package ui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/fmidev/kubetin/internal/model"
)

func TestFitColumns(t *testing.T) {
	cols := []column{
		{min: 10, max: 20, prio: 0},
		{min: 5, max: 5, prio: 1},
		{min: 8, max: 8, prio: 2},
	}

	check := func(name string, avail int, want []int) {
		t.Helper()
		got := fitColumns(cols, avail)
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: fitColumns(_, %d) = %v, want %v", name, avail, got, want)
				return
			}
		}
	}

	// mins are 10+5+8 plus two gaps = 27; leftover feeds col 0 to max.
	check("grow-to-max", 40, []int{20, 5, 8})
	// Exactly minimums.
	check("exact-fit", 27, []int{10, 5, 8})
	// Too narrow: highest prio dropped first, leftover redistributed.
	check("drop-one", 20, []int{13, 5, 0})
	// Narrower still: only the prio-0 column survives, at its min even
	// if that overflows avail (clampCanvas truncates downstream).
	check("drop-all-but-essential", 4, []int{10, 0, 0})
}

func TestJoinCellsSkipsDropped(t *testing.T) {
	if got := joinCells("aa", "", "bb"); got != "aa  bb" {
		t.Fatalf("joinCells = %q, want %q", got, "aa  bb")
	}
	if got := joinCells("", "aa"); got != "aa" {
		t.Fatalf("joinCells leading drop = %q, want %q", got, "aa")
	}
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// headerAt renders the pod table at the given pane width and returns
// the header line with ANSI stripped.
func podHeaderAt(t *testing.T, width int) string {
	t.Helper()
	m := New("alpha", model.NewStore(), []string{"alpha"})
	m.width, m.height = width, 30
	out := m.renderTable(20, width)
	return ansiRE.ReplaceAllString(strings.SplitN(out, "\n", 2)[0], "")
}

// The pod table must degrade by dropping low-priority columns on
// narrow panes and keep the full set on wide ones.
func TestPodTableResponsiveColumns(t *testing.T) {
	wide := podHeaderAt(t, 170)
	for _, label := range []string{"NAMESPACE", "POD", "STATUS", "CONTAINERS", "RESTARTS", "AGE", "CPU", "MEM", "NET", "NODE"} {
		if !strings.Contains(wide, label) {
			t.Errorf("wide header missing %q: %q", label, wide)
		}
	}

	narrow := podHeaderAt(t, 80)
	for _, label := range []string{"NAMESPACE", "POD", "STATUS", "CPU", "MEM"} {
		if !strings.Contains(narrow, label) {
			t.Errorf("80-cell header missing %q: %q", label, narrow)
		}
	}
	for _, label := range []string{"NET", "NODE"} {
		if strings.Contains(narrow, label) {
			t.Errorf("80-cell header should have dropped %q: %q", label, narrow)
		}
	}

	tiny := podHeaderAt(t, 60)
	for _, label := range []string{"NAMESPACE", "POD", "STATUS", "CPU"} {
		if !strings.Contains(tiny, label) {
			t.Errorf("60-cell header missing %q: %q", label, tiny)
		}
	}
	if strings.Contains(tiny, "MEM") {
		t.Errorf("60-cell header should have dropped MEM: %q", tiny)
	}
}
