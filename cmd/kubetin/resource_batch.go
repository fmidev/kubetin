package main

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fmidev/kubetin/internal/ui"
)

const resourceBatchMax = 64

// Drain only events already available: quiet watches gain no timer latency,
// while startup bursts require one render per bounded batch instead of per row.
func forwardResourceEvents[T any](ctx context.Context, out <-chan T, p watchMessageSender, focus ui.FocusTarget, wrap func(T) tea.Msg) {
	for {
		var first T
		select {
		case <-ctx.Done():
			return
		case event, ok := <-out:
			if !ok {
				return
			}
			first = event
		}
		batch := ui.ResourceBatchMsg{wrap(first)}
		closed := false
	drain:
		for len(batch) < resourceBatchMax {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-out:
				if !ok {
					closed = true
					break drain
				}
				batch = append(batch, wrap(event))
			default:
				break drain
			}
		}
		p.Send(ui.FocusedMsg{Focus: focus, Msg: batch})
		if closed {
			return
		}
	}
}
