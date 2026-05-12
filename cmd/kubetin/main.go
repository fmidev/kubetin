// Command kubetin is the multi-cluster Kubernetes terminal monitor.
//
// Default mode is the bubbletea TUI showing pods for one focused
// context. -headless preserves the W1 spike behaviour: print probe
// status (and optional pod events) on stdout. The probe loop runs
// against every kubeconfig context in either mode.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"k8s.io/klog/v2"

	"github.com/fmidev/kubetin/internal/cluster"
	"github.com/fmidev/kubetin/internal/model"
	"github.com/fmidev/kubetin/internal/ui"
)

func main() {
	probeInterval := flag.Duration("probe", 30*time.Second, "per-cluster probe interval")
	printInterval := flag.Duration("print", 5*time.Second, "headless status print interval")
	watchCtx := flag.String("watch", "", "context to watch pods on (empty = first healthy)")
	headless := flag.Bool("headless", false, "no TUI; print status + pod events to stdout")
	noWatch := flag.Bool("no-watch", false, "skip pod informer; only run reachability probes")
	showVersion := flag.Bool("version", false, "print version and exit")
	trustAll := flag.Bool("trust", false, "add every discovered kubeconfig to the trust list and exit")
	flag.Parse()

	if *showVersion {
		printVersion()
		return
	}

	if *trustAll {
		if err := runTrust(); err != nil {
			fmt.Fprintf(os.Stderr, "kubetin: -trust: %v\n", err)
			os.Exit(1)
		}
		return
	}

	logFile, err := openDebugLog()
	if err == nil {
		klog.SetOutput(logFile)
		klog.LogToStderr(false)
		defer logFile.Close()
	} else {
		klog.SetOutput(io.Discard)
		klog.LogToStderr(false)
	}
	// Stamp every session with the build identity so a colleague who
	// hands us a debug.log can tell which kubetin produced it.
	klog.Infof("startup: %s", versionString())

	// Run discovery (and any "file X is untrusted" warnings or
	// first-run prompts) BEFORE silencing stderr — otherwise their
	// output goes straight to /dev/null and the user has no way to
	// understand why a kubeconfig they expected isn't loading.
	// `oc login` is the canonical case: it rewrites ~/.kube/config
	// with a new token, the sha256 changes, kubetin refuses the
	// (now-untrusted) content, and silently dropping the warning
	// makes the cluster look like it just vanished.
	d, err := loadTrustedDiscovery()
	if err != nil {
		fmt.Fprintf(os.Stderr, "kubetin: %v\n", err)
		os.Exit(1)
	}

	// In TUI mode, replace fd 2 with /dev/null so exec credential
	// plugins (gke-gcloud-auth-plugin, aws-iam-auth, kubelogin) — which
	// inherit cmd.Stderr=os.Stderr by default — don't tear through the
	// alt-screen and don't pollute debug.log with vendor-formatted
	// diagnostics. We keep the original fd around so the panic-recover
	// path can restore it before printing the crash stack. The headless
	// path is left alone: stderr is the right place for headless errors.
	restoreStderr := func() {}
	if !*headless {
		if r, sErr := silenceStderr(); sErr == nil {
			restoreStderr = r
		} else {
			klog.Warningf("silenceStderr failed: %v", sErr)
		}
	}

	store := model.NewStore()
	sup := cluster.New(d, store, *probeInterval)

	rootCtx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go sup.Run(rootCtx)

	if *headless {
		runHeadless(rootCtx, store, sup, d.Contexts, *printInterval, *watchCtx, *noWatch)
		return
	}
	runTUI(rootCtx, store, sup, d.Contexts, *watchCtx, *noWatch, restoreStderr)
}

func openDebugLog() (*os.File, error) {
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		state = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(state, "kubetin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// 0o600 — file is the only audit trail for delete + secret-reveal
	// breadcrumbs and inherits exec-plugin stderr; must not be
	// readable by other local users on shared workstations.
	path := filepath.Join(dir, "debug.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	// Tighten existing files that were created at 0o644 by older builds.
	_ = os.Chmod(path, 0o600)
	return f, nil
}

// runTUI launches the bubbletea program with a panic-safe terminal
// restore, then bridges the pod watcher into Program.Send. A single
// coordinator goroutine owns the active watcher so focus switches can
// cancel and replace it without race conditions.
func runTUI(ctx context.Context, store *model.Store, sup *cluster.Supervisor, contexts []string, want string, noWatch bool, restoreStderr func()) {
	selected, err := pickWatchContext(ctx, store, want, contexts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kubetin: %v\n", err)
		os.Exit(1)
	}

	m := ui.New(selected, store, contexts)
	m.Build = versionString()

	// prog is created later but referenced by callbacks defined now.
	// Callbacks fire only after prog.Run starts, so the nil check is
	// belt-and-braces.
	var prog *tea.Program

	// Build the watch coordinator first so we can wire its switchTo
	// into the model before bubbletea takes ownership.
	var coord *watchCoordinator
	if !noWatch {
		coord = &watchCoordinator{
			parent: ctx,
			sup:    sup,
			reqCh:  make(chan string, 4),
			stopCh: make(chan struct{}),
			doneCh: make(chan struct{}),
		}
		m.OnFocusChange = coord.switchTo
	}

	// Wire describe so the UI can fetch live YAML via the supervisor
	// without importing it.
	m.OnDescribe = func(req ui.DescribeRequestMsg, focusedCtx string) tea.Msg {
		if req.Reveal && req.Ref.Kind == "Secret" {
			klog.Infof("describe: revealing Secret/%s in ns/%s on context %s",
				req.Ref.Name, req.Ref.Namespace, focusedCtx)
		}
		fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		res := sup.Describe(fetchCtx, focusedCtx, req.Ref, req.Reveal)
		return ui.DescribeResultMsg(res)
	}

	// Wire SSAR so the UI can hide actions the user can't perform.
	m.OnCanI = func(ctxName, verb, group, resource, namespace string) tea.Msg {
		fetchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		res := sup.CanI(fetchCtx, ctxName, verb, group, resource, namespace)
		key := cluster.PermissionKey(ctxName, verb, group, resource, namespace)
		// We treat "Err" as "no, I can't" — surfacing it as "allowed=true"
		// would expose a button that crashes when used. Reason/Err flow
		// through so the RBAC overlay can show *why* an action was
		// denied instead of just "no".
		return ui.PermissionResultMsg{
			Context: ctxName,
			Key:     key,
			Allowed: res.Allowed && res.Err == "",
			Reason:  res.Reason,
			Err:     res.Err,
		}
	}

	// Wire logs streaming. We hold a single cancellable context per
	// stream — opening a new one cancels the old.
	var (
		logCancel context.CancelFunc
		logMu     sync.Mutex
	)
	stopLogs := func() {
		logMu.Lock()
		defer logMu.Unlock()
		if logCancel != nil {
			logCancel()
			logCancel = nil
		}
	}

	m.OnLogsStart = func(focusedCtx string, req ui.LogStartMsg) tea.Msg {
		// Stop any stream already running.
		stopLogs()
		streamCtx, cancel := context.WithCancel(ctx)
		logMu.Lock()
		logCancel = cancel
		logMu.Unlock()

		ls, err := sup.StreamLogs(streamCtx, focusedCtx,
			req.Ref.Namespace, req.Ref.Name, req.Container, 200)
		if err != nil {
			cancel()
			return ui.LogErrorMsg{Session: req.Session, Err: err.Error()}
		}

		// Forwarder goroutine: drain ls.Out into the bubbletea program.
		// Lines are batched up to logBatchMax or logBatchWindow, then
		// shipped as a single LogLinesMsg. This keeps the event loop
		// quiet on chatty pods (think kube-system flannel/calico) where
		// per-line Send()s cost more in lock contention than the
		// rendering itself. EOS/Err flush any pending batch first so
		// the user never loses tail context to a buffered shutdown.
		// Session id is echoed on every emitted message so the UI can
		// drop late lines from a previously-cancelled stream.
		go forwardLogs(ls, prog, req.Session)
		return nil
	}
	m.OnLogsStop = stopLogs
	// Make sure we stop the stream on shutdown.
	defer stopLogs()

	// Wire exec. The callback returns a tea.Cmd rather than a tea.Msg
	// because exec needs the program to release the alt-screen for
	// the lifetime of the session, which is exactly what tea.Exec
	// provides. The audit log lands two lines: one when kubetin
	// requests the session (before the apiserver call), one when the
	// session ends with the outcome. NewExecCmd failing means we
	// never reached the apiserver — short-circuit with ExecDoneMsg
	// so the UI surfaces the error without an empty terminal flicker.
	m.OnExec = func(focusedCtx string, ref cluster.DescribeRef, container string, command []string) tea.Cmd {
		klog.Infof("exec: %s container=%s in ns/%s on %s cmd=%v requested",
			ref.Name, container, ref.Namespace, focusedCtx, command)
		execCmd, err := sup.NewExecCmd(focusedCtx, ref.Namespace, ref.Name, container, command)
		if err != nil {
			klog.Errorf("exec: %s container=%s on %s setup FAILED: %v",
				ref.Name, container, focusedCtx, err)
			return func() tea.Msg { return ui.ExecDoneMsg{Err: err} }
		}
		return tea.Exec(execCmd, func(execErr error) tea.Msg {
			if execErr != nil {
				klog.Errorf("exec: %s container=%s on %s ended with error: %v",
					ref.Name, container, focusedCtx, execErr)
			} else {
				klog.Infof("exec: %s container=%s on %s ended cleanly",
					ref.Name, container, focusedCtx)
			}
			return ui.ExecDoneMsg{Err: execErr}
		})
	}

	// Wire scale.
	m.OnScale = func(focusedCtx string, ref cluster.DescribeRef, replicas int32) tea.Msg {
		klog.Infof("scale: %s/%s in ns/%s on %s → %d requested",
			ref.Kind, ref.Name, ref.Namespace, focusedCtx, replicas)
		fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		res := sup.Scale(fetchCtx, focusedCtx, ref, replicas)
		if res.OK {
			klog.Infof("scale: %s/%s on %s → %d OK",
				ref.Kind, ref.Name, focusedCtx, res.Replicas)
		} else {
			klog.Errorf("scale: %s/%s on %s FAILED: %s",
				ref.Kind, ref.Name, focusedCtx, res.Err)
		}
		return ui.ScaleResultMsg(res)
	}

	// Wire rollout restart.
	m.OnRolloutRestart = func(focusedCtx string, ref cluster.DescribeRef) tea.Msg {
		klog.Infof("rollout-restart: %s/%s in ns/%s on %s requested",
			ref.Kind, ref.Name, ref.Namespace, focusedCtx)
		fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		res := sup.RolloutRestart(fetchCtx, focusedCtx, ref)
		if res.OK {
			klog.Infof("rollout-restart: %s/%s on %s OK",
				ref.Kind, ref.Name, focusedCtx)
		} else {
			klog.Errorf("rollout-restart: %s/%s on %s FAILED: %s",
				ref.Kind, ref.Name, focusedCtx, res.Err)
		}
		return ui.RolloutResultMsg(res)
	}

	// Wire delete with audit log.
	m.OnDelete = func(focusedCtx string, ref cluster.DescribeRef) tea.Msg {
		klog.Infof("delete: %s/%s in ns/%s on context %s requested",
			ref.Kind, ref.Name, ref.Namespace, focusedCtx)
		delCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		res := sup.Delete(delCtx, focusedCtx, ref)
		if res.OK {
			klog.Infof("delete: %s/%s in ns/%s on %s OK",
				ref.Kind, ref.Name, ref.Namespace, focusedCtx)
		} else {
			klog.Errorf("delete: %s/%s in ns/%s on %s FAILED: %s",
				ref.Kind, ref.Name, ref.Namespace, focusedCtx, res.Err)
		}
		return ui.DeleteResultMsg(res)
	}

	// Node maintenance — Cordon, Uncordon, Drain.
	//
	// Cordon and Uncordon are one-shot PATCHes; we audit-log the
	// request and the result, and that's the whole story. Drain is
	// async because the eviction loop can take minutes against busy
	// nodes — we hand the supervisor a parent context the UI can
	// cancel, kick the Drain goroutine, and forward each progress
	// event to the bubbletea program from a small fan-out goroutine.
	m.OnCordon = func(focusedCtx, node string) tea.Msg {
		klog.Infof("cordon: %s on %s requested", node, focusedCtx)
		opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		res := sup.Cordon(opCtx, focusedCtx, node)
		if res.OK {
			klog.Infof("cordon: %s on %s OK", node, focusedCtx)
		} else {
			klog.Errorf("cordon: %s on %s FAILED: %s", node, focusedCtx, res.Err)
		}
		return ui.NodeOpResultMsg(res)
	}
	m.OnUncordon = func(focusedCtx, node string) tea.Msg {
		klog.Infof("uncordon: %s on %s requested", node, focusedCtx)
		opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		res := sup.Uncordon(opCtx, focusedCtx, node)
		if res.OK {
			klog.Infof("uncordon: %s on %s OK", node, focusedCtx)
		} else {
			klog.Errorf("uncordon: %s on %s FAILED: %s", node, focusedCtx, res.Err)
		}
		return ui.NodeOpResultMsg(res)
	}
	m.OnDrainStart = func(focusedCtx, node string) tea.Msg {
		klog.Infof("drain: %s on %s requested", node, focusedCtx)
		// Drain context is detached from the per-request timeout —
		// the UI cancels via the function we return on the
		// DrainStartMsg, not via a wall-clock deadline. ctx is the
		// process-level context, so an outer shutdown still aborts.
		drainCtx, cancel := context.WithCancel(ctx)
		progress := make(chan cluster.DrainProgress, 64)

		go sup.Drain(drainCtx, focusedCtx, node, progress)

		// Forwarder goroutine — drains the channel into the
		// bubbletea program. We collect "blocked" pods locally so
		// we can hand the final list to DrainDoneMsg without making
		// the UI re-walk its own append-only list.
		go func() {
			var blocked []string
			var finalDone, finalTotal int
			var finalErr string
			for ev := range progress {
				prog.Send(ui.DrainProgressMsg(ev))
				if ev.Total > finalTotal {
					finalTotal = ev.Total
				}
				if ev.Done > finalDone {
					finalDone = ev.Done
				}
				if ev.Phase == "blocked" {
					blocked = append(blocked, ev.Pod+" ("+ev.Err+")")
				}
				if ev.Phase == "error" {
					finalErr = ev.Err
				}
			}
			klog.Infof("drain: %s on %s ended done=%d/%d err=%q blocked=%d",
				node, focusedCtx, finalDone, finalTotal, finalErr, len(blocked))
			prog.Send(ui.DrainDoneMsg{
				Context: focusedCtx,
				Node:    node,
				Done:    finalDone,
				Total:   finalTotal,
				Err:     finalErr,
				Blocked: blocked,
			})
			cancel() // releases the drain context once the stream is fully drained
		}()

		return ui.DrainStartMsg{
			Context: focusedCtx,
			Node:    node,
			Cancel:  cancel,
		}
	}

	// No mouse capture: kubetin does not use mouse events yet, and
	// enabling it disables the terminal's native click-and-drag text
	// selection, breaking copy-paste. Re-enable with WithMouseClicks
	// or WithMouseCellMotion when we have something for it to do.
	prog = tea.NewProgram(m, tea.WithAltScreen())

	if coord != nil {
		coord.prog = prog
		go coord.loop()
		coord.switchTo(selected)
	}

	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			// Persist the crash to debug.log too. The user's terminal
			// scrollback is the obvious place but it's lossy: many
			// terminals truncate, and "paste your crash" is friendlier
			// when there's a stable file path to attach.
			klog.Errorf("panic: %v\n%s", r, stack)
			prog.ReleaseTerminal()
			// Put real stderr back before writing — otherwise the crash
			// stack disappears into /dev/null and the user sees nothing.
			restoreStderr()
			fmt.Fprintf(os.Stderr, "kubetin: panic: %v\n%s\n", r, stack)
			fmt.Fprintf(os.Stderr, "build: %s\n", versionString())
			os.Exit(2)
		}
	}()

	if _, err := prog.Run(); err != nil {
		// Same reasoning as panic recover: restore real stderr so the
		// user sees why the TUI exited instead of getting a silent
		// non-zero status.
		restoreStderr()
		fmt.Fprintf(os.Stderr, "kubetin: tui: %v\n", err)
		os.Exit(1)
	}
	if coord != nil {
		coord.stop()
	}
}

// watchCoordinator owns the currently-active PodWatcher. switchTo
// cancels the old watcher (if any) and starts a new one for ctx.
// Calls are serialised through reqCh so concurrent Tab presses don't
// race each other.
type watchCoordinator struct {
	parent context.Context
	sup    *cluster.Supervisor
	prog   *tea.Program

	reqCh  chan string
	stopCh chan struct{}
	doneCh chan struct{}
}

func (c *watchCoordinator) switchTo(target string) {
	// Non-blocking send into a buffered channel. The loop debounces;
	// a backed-up channel just means the loop will pick the latest
	// target on the next iteration.
	select {
	case c.reqCh <- target:
	default:
	}
}

func (c *watchCoordinator) stop() {
	close(c.stopCh)
	<-c.doneCh
}

// debounceWindow is the minimum time a focus selection must "stick"
// before we tear down the existing watchers and start new ones for it.
// Rapid Tab/Shift-Tab presses through a long context list otherwise
// kick off a fresh LIST/WATCH against every cluster the cursor passes
// through — costly on big clusters and pointless because the user is
// almost never asking for those intermediate cluster's data. 250ms
// matches typical typeahead pacing without feeling laggy on a single
// deliberate press.
const debounceWindow = 250 * time.Millisecond

func (c *watchCoordinator) loop() {
	defer close(c.doneCh)
	var (
		curCancel context.CancelFunc
		pending   string
	)
	// Pre-stopped timer; we only Reset() it when there's something
	// queued. Drain on Reset to avoid stale fires.
	debounce := time.NewTimer(time.Hour)
	if !debounce.Stop() {
		<-debounce.C
	}
	defer debounce.Stop()

	apply := func(target string) {
		if curCancel != nil {
			curCancel()
		}
		child, cancel := context.WithCancel(c.parent)
		curCancel = cancel
		c.spawn(child, target)
	}

	for {
		select {
		case <-c.parent.Done():
			if curCancel != nil {
				curCancel()
			}
			return
		case <-c.stopCh:
			if curCancel != nil {
				curCancel()
			}
			return
		case target := <-c.reqCh:
			pending = target
			if !debounce.Stop() {
				select {
				case <-debounce.C:
				default:
				}
			}
			debounce.Reset(debounceWindow)
		case <-debounce.C:
			if pending == "" {
				continue
			}
			target := pending
			pending = ""
			apply(target)
		}
	}
}

// spawn starts the per-cluster watcher set inside the given child ctx.
// Extracted from loop() so the request/debounce flow stays readable.
func (c *watchCoordinator) spawn(child context.Context, target string) {
	pw := cluster.NewPodWatcher(target, 256)
	go forwardPodEvents(child, pw, c.prog)
	go func() {
		if err := pw.Run(child, c.sup); err != nil {
			klog.Errorf("pod watcher (%s) exited: %v", target, err)
		}
	}()

	nw := cluster.NewNodeWatcher(target, 64)
	go forwardNodeEvents(child, nw, c.prog)
	go func() {
		if err := nw.Run(child, c.sup); err != nil {
			klog.Errorf("node watcher (%s) exited: %v", target, err)
		}
	}()

	dw := cluster.NewDeployWatcher(target, 256)
	go forwardDeployEvents(child, dw, c.prog)
	go func() {
		if err := dw.Run(child, c.sup); err != nil {
			klog.Errorf("deploy watcher (%s) exited: %v", target, err)
		}
	}()

	ew := cluster.NewEventWatcher(target, 512)
	go forwardEvtEvents(child, ew, c.prog)
	go func() {
		if err := ew.Run(child, c.sup); err != nil {
			klog.Errorf("event watcher (%s) exited: %v", target, err)
		}
	}()

	// Namespace watcher is cluster-scoped; on namespace-restricted
	// users (typical OpenShift project) the watcher itself no-ops
	// via the Supervisor.ResolveScope check inside Run(), so we can
	// always spawn it — RBAC checks live one layer down rather than
	// being duplicated here.
	nsw := cluster.NewNamespaceWatcher(target, 256)
	go forwardNsEvents(child, nsw.Out, c.prog)
	go func() {
		if err := nsw.Run(child, c.sup); err != nil {
			klog.Errorf("namespace watcher (%s) exited: %v", target, err)
		}
	}()

	// Project watcher (OpenShift). Self-gates inside Run(): no-ops
	// when the cluster doesn't expose project.openshift.io/v1 or the
	// user can't list projects. When it IS the source of truth, the
	// namespace watcher above no-ops via its own projectsPreferred
	// check, so the two are mutually exclusive at runtime.
	projw := cluster.NewProjectWatcher(target, 256)
	go forwardNsEvents(child, projw.Out, c.prog)
	go func() {
		if err := projw.Run(child, c.sup); err != nil {
			klog.Errorf("project watcher (%s) exited: %v", target, err)
		}
	}()

	mp := cluster.NewFocusedMetricsPoller(target, 4)
	go forwardMetrics(child, mp, c.prog)
	go func() {
		if err := mp.Run(child, c.sup); err != nil {
			klog.Errorf("metrics poller (%s) exited: %v", target, err)
		}
	}()

	// Network rates come from cAdvisor on each kubelet, scraped via
	// the apiserver proxy. Independent from metrics-server, so it can
	// run standalone — clusters without metrics-server still get
	// network panels (and vice versa).
	np := cluster.NewNetworkPoller(target, 4)
	go forwardNetwork(child, np, c.prog)
	go func() {
		if err := np.Run(child, c.sup); err != nil {
			klog.Errorf("network poller (%s) exited: %v", target, err)
		}
	}()
}

func forwardPodEvents(ctx context.Context, w *cluster.PodWatcher, p *tea.Program) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-w.Out:
			p.Send(ui.PodEventMsg(ev))
		}
	}
}

func forwardNodeEvents(ctx context.Context, w *cluster.NodeWatcher, p *tea.Program) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-w.Out:
			p.Send(ui.NodeEventMsg(ev))
		}
	}
}

func forwardDeployEvents(ctx context.Context, w *cluster.DeployWatcher, p *tea.Program) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-w.Out:
			p.Send(ui.DeployEventMsg(ev))
		}
	}
}

func forwardEvtEvents(ctx context.Context, w *cluster.EventWatcher, p *tea.Program) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-w.Out:
			p.Send(ui.EvtEventMsg(ev))
		}
	}
}

// forwardNsEvents pumps NamespaceEvents (from either NamespaceWatcher
// or ProjectWatcher — both emit the same shape) into the bubbletea
// program. Taking the channel directly rather than the concrete
// watcher type keeps the forwarder shared.
func forwardNsEvents(ctx context.Context, out <-chan cluster.NamespaceEvent, p *tea.Program) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-out:
			p.Send(ui.NsEventMsg(ev))
		}
	}
}

// Log forwarding constants. logBatchWindow is short enough that the
// view feels live (50ms is two render frames at 30fps) and large
// enough that high-rate pods get bundled. logBatchMax bounds the
// per-message slice so we don't blow up Update with an enormous
// append on a sustained log storm.
const (
	logBatchWindow = 50 * time.Millisecond
	logBatchMax    = 64
)

func forwardLogs(ls *cluster.LogStreamer, p *tea.Program, session uint64) {
	if p == nil {
		return
	}
	var batch []string
	timer := time.NewTimer(logBatchWindow)
	if !timer.Stop() {
		<-timer.C
	}
	timerArmed := false

	flush := func() {
		if len(batch) == 0 {
			return
		}
		// Hand the slice off and replace it. Reusing the underlying
		// array would race with the receiving goroutine inside Update.
		p.Send(ui.LogLinesMsg{Session: session, Lines: batch})
		batch = nil
	}
	armTimer := func() {
		if !timerArmed {
			timer.Reset(logBatchWindow)
			timerArmed = true
		}
	}
	disarmTimer := func() {
		if timerArmed {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timerArmed = false
		}
	}

	for {
		select {
		case line, ok := <-ls.Out:
			if !ok {
				disarmTimer()
				flush()
				return
			}
			switch {
			case line.EOS:
				disarmTimer()
				flush()
				p.Send(ui.LogEOSMsg{Session: session})
			case line.Reconnecting:
				// Transient: streamer will retry. Err on the same
				// record carries the cause for debug.log only — UI
				// shows a "reconnecting" indicator, not the fatal ✕.
				disarmTimer()
				flush()
				p.Send(ui.LogReconnectingMsg{Session: session, Cause: line.Err})
			case line.Err != "":
				disarmTimer()
				flush()
				p.Send(ui.LogErrorMsg{Session: session, Err: line.Err})
			default:
				batch = append(batch, line.Line)
				if len(batch) >= logBatchMax {
					disarmTimer()
					flush()
				} else {
					armTimer()
				}
			}
		case <-timer.C:
			timerArmed = false
			flush()
		}
	}
}

func forwardNetwork(ctx context.Context, np *cluster.NetworkPoller, p *tea.Program) {
	for {
		select {
		case <-ctx.Done():
			return
		case snap := <-np.Out:
			p.Send(ui.NetworkSnapshotMsg(snap))
		}
	}
}

func forwardMetrics(ctx context.Context, mp *cluster.FocusedMetricsPoller, p *tea.Program) {
	for {
		select {
		case <-ctx.Done():
			return
		case snap := <-mp.Out:
			p.Send(ui.MetricsSnapshotMsg(snap))
		}
	}
}

// runHeadless preserves the W1 spike behaviour for soak testing.
func runHeadless(ctx context.Context, store *model.Store, sup *cluster.Supervisor, contexts []string, printEvery time.Duration, want string, noWatch bool) {
	fmt.Printf("kubetin headless: %d contexts · probe loop running\n\n", len(contexts))

	// watcher / watchedCtx are written by the watch goroutine and read
	// by the print-loop goroutine — guarded by mu so `go test -race`
	// stays clean and so string-word tearing can't happen on weak-
	// memory archs.
	var (
		mu         sync.Mutex
		watcher    *cluster.PodWatcher
		watchedCtx string
		podCount   atomic.Int64
		watchDone  = make(chan struct{})
	)

	current := func() (*cluster.PodWatcher, string) {
		mu.Lock()
		defer mu.Unlock()
		return watcher, watchedCtx
	}

	if !noWatch {
		go func() {
			defer close(watchDone)
			selected, err := pickWatchContext(ctx, store, want, contexts)
			if err != nil {
				fmt.Fprintf(os.Stderr, "kubetin: -watch: %v\n", err)
				return
			}
			pw := cluster.NewPodWatcher(selected, 256)

			mu.Lock()
			watchedCtx = selected
			watcher = pw
			mu.Unlock()

			fmt.Printf(">> watching pods on context %q\n\n", selected)
			go consumePodEvents(ctx, pw, &podCount)

			if err := pw.Run(ctx, sup); err != nil {
				fmt.Fprintf(os.Stderr, "kubetin: pod watcher (%s) exited: %v\n", selected, err)
			}
		}()
	} else {
		close(watchDone)
	}

	pt := time.NewTicker(printEvery)
	defer pt.Stop()
	w, wc := current()
	printStatus(store, wc, &podCount, w)
	for {
		select {
		case <-ctx.Done():
			fmt.Println("\nkubetin: shutting down")
			<-watchDone
			return
		case <-pt.C:
			w, wc := current()
			printStatus(store, wc, &podCount, w)
		}
	}
}

func pickWatchContext(ctx context.Context, store *model.Store, want string, contexts []string) (string, error) {
	if want != "" {
		for _, c := range contexts {
			if c == want {
				return want, nil
			}
		}
		return "", fmt.Errorf("context %q not found in kubeconfig", want)
	}
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		for _, c := range contexts {
			if st, ok := store.Get(c); ok && st.Reach == model.ReachHealthy {
				return c, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline.C:
			return "", fmt.Errorf("no healthy cluster appeared within 30s; pass -watch <ctx>")
		case <-tick.C:
		}
	}
}

func consumePodEvents(ctx context.Context, w *cluster.PodWatcher, count *atomic.Int64) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-w.Out:
			switch ev.Kind {
			case cluster.PodAdded:
				count.Add(1)
			case cluster.PodDeleted:
				count.Add(-1)
			}
			fmt.Printf("   %s  %s  %s/%-40s  phase=%-10s  restarts=%d  node=%s\n",
				time.Now().Format("15:04:05"),
				ev.Kind, ev.Namespace, ev.Name, ev.Phase, ev.Restarts, ev.NodeName)
		}
	}
}

func printStatus(s *model.Store, watched string, podCount *atomic.Int64, w *cluster.PodWatcher) {
	snap := s.Snapshot()
	sort.Slice(snap, func(i, j int) bool { return snap[i].Context < snap[j].Context })

	now := time.Now().Format("15:04:05")
	suffix := ""
	if watched != "" {
		dropped := uint64(0)
		if w != nil {
			dropped = w.DroppedEvents.Load()
		}
		suffix = fmt.Sprintf(" · watching=%s pods=%d dropped=%d", watched, podCount.Load(), dropped)
	}
	fmt.Printf("── %s ── %d clusters%s ───────────────────────────\n", now, len(snap), suffix)
	for _, st := range snap {
		ageStr := ""
		if !st.LastProbe.IsZero() {
			ageStr = fmt.Sprintf("%2ds ago", int(time.Since(st.LastProbe).Seconds()))
		}
		latency := ""
		if st.ProbeLatency > 0 {
			latency = fmt.Sprintf("%dms", st.ProbeLatency.Milliseconds())
		}
		ver := st.ServerVersion
		if ver == "" {
			ver = "—"
		}
		errStr := ""
		if st.LastError != "" {
			errStr = " · " + st.LastError
		}
		marker := " "
		if st.Context == watched {
			marker = "›"
		}
		fmt.Printf("%s%s %-22s %-12s %-16s %7s %7s%s\n",
			marker, st.Reach.Glyph(), st.Context, st.Reach.String(), ver, latency, ageStr, errStr)
	}
	fmt.Println()
}
