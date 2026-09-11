package ui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
)

// Each allocation identifies one modal operation, even after closing and
// reopening the same resource. The context also invalidates queued work.
type modalRequest struct {
	ctx    context.Context
	cancel context.CancelFunc
}

type modalResultMsg struct {
	request *modalRequest
	msg     tea.Msg
}

func newModalRequest(parent context.Context) *modalRequest {
	ctx, cancel := context.WithCancel(parent)
	return &modalRequest{ctx: ctx, cancel: cancel}
}

func (r *modalRequest) stop() {
	if r != nil {
		r.cancel()
	}
}

func (r *modalRequest) accepts(result *modalRequest) bool {
	return r != nil && r == result && r.ctx.Err() == nil
}

func (m Model) modalCmd(request *modalRequest, cmd tea.Cmd) tea.Cmd {
	return m.focusedCmd(func() tea.Msg {
		if request.ctx.Err() != nil {
			return nil
		}
		return modalResultMsg{request: request, msg: cmd()}
	})
}
