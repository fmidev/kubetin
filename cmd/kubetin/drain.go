package main

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
	"k8s.io/klog/v2"

	"github.com/fmidev/kubetin/internal/cluster"
	"github.com/fmidev/kubetin/internal/ui"
)

func startDrain(parent context.Context, req ui.DrainRequest, run func(context.Context, string, string, chan<- cluster.DrainProgress), send func(tea.Msg)) tea.Msg {
	drainCtx, cancel := context.WithCancel(req.Context)
	stopShutdown := context.AfterFunc(parent, cancel)
	if err := req.Context.Err(); err != nil || parent.Err() != nil {
		cancel()
		stopShutdown()
		return ui.DrainStartMsg{Context: req.Focus.Context, Node: req.Node, Err: "drain cancelled before startup"}
	}
	klog.Infof("drain: %s on %s requested", req.Node, req.Focus.Context)
	progress := make(chan cluster.DrainProgress, 64)
	go run(drainCtx, req.Focus.Context, req.Node, progress)

	go func() {
		defer cancel()
		defer stopShutdown()
		publish := func(msg tea.Msg) {
			send(ui.FocusedMsg{Focus: req.Focus, Msg: ui.DrainMsg{Session: req.Session, Msg: msg}})
		}
		var blocked, remaining []string
		var finalDone, finalTotal int
		var finalErr string
		for ev := range progress {
			publish(ui.DrainProgressMsg(ev))
			finalTotal = max(finalTotal, ev.Total)
			finalDone = max(finalDone, ev.Done)
			if ev.Phase == "blocked" {
				blocked = append(blocked, ev.Pod+" ("+ev.Err+")")
			}
			if ev.Phase == "error" {
				finalErr = ev.Err
			}
			if ev.Phase == "error" || ev.Phase == "done" {
				remaining = ev.Remaining
			}
		}
		klog.Infof("drain: %s on %s ended done=%d/%d err=%q blocked=%d",
			req.Node, req.Focus.Context, finalDone, finalTotal, finalErr, len(blocked))
		publish(ui.DrainDoneMsg{
			Context: req.Focus.Context, Node: req.Node, Done: finalDone, Total: finalTotal,
			Err: finalErr, Blocked: blocked, Remaining: remaining,
		})
	}()

	return ui.DrainStartMsg{Context: req.Focus.Context, Node: req.Node, Cancel: cancel}
}
