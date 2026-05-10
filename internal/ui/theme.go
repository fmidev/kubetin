package ui

import (
	"github.com/charmbracelet/lipgloss"

	corev1 "k8s.io/api/core/v1"
)

// Theme bundles the lipgloss styles used across the UI. Kept tiny on
// purpose — anything that grows past trivial gets its own file.
type Theme struct {
	Base      lipgloss.Style
	Dim       lipgloss.Style
	Header    lipgloss.Style
	Footer    lipgloss.Style
	Selected  lipgloss.Style
	Title     lipgloss.Style
	StatusOK  lipgloss.Style
	StatusBad lipgloss.Style
	StatusWrn lipgloss.Style
	StatusDim lipgloss.Style
}

// DefaultTheme is the truecolor theme. We do not yet branch on
// COLORTERM — that's a v0.5 concern.
func DefaultTheme() Theme {
	return Theme{
		Base:      lipgloss.NewStyle(),
		Dim:       lipgloss.NewStyle().Foreground(lipgloss.Color("244")),
		Header:    lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#dddddd")),
		Footer:    lipgloss.NewStyle().Foreground(lipgloss.Color("244")),
		Selected:  lipgloss.NewStyle().Background(lipgloss.Color("#3a3a3a")),
		Title:     lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7dd3fc")),
		StatusOK:  lipgloss.NewStyle().Foreground(lipgloss.Color("#4ade80")),
		StatusBad: lipgloss.NewStyle().Foreground(lipgloss.Color("#f87171")),
		StatusWrn: lipgloss.NewStyle().Foreground(lipgloss.Color("#fbbf24")),
		StatusDim: lipgloss.NewStyle().Foreground(lipgloss.Color("244")),
	}
}

// styleForPhase returns the right colour style for a Pod phase.
func (t Theme) styleForPhase(p corev1.PodPhase) lipgloss.Style {
	switch p {
	case corev1.PodRunning:
		return t.StatusOK
	case corev1.PodPending:
		return t.StatusWrn
	case corev1.PodFailed:
		return t.StatusBad
	case corev1.PodSucceeded:
		return t.StatusDim
	}
	return t.Base
}
