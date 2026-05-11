// PTY-aware interactive exec into a pod container.
//
// We deliberately don't shell out to `kubectl exec`. The kubetin
// contract is "single static binary, no runtime deps beyond a
// kubeconfig" — adding kubectl as a hard requirement would break it.
// Instead we drive the apiserver's pods/exec subresource directly
// via client-go's remotecommand.SPDYExecutor, and hand the local
// terminal over by implementing bubbletea's tea.ExecCommand
// interface so the program suspends its alt-screen for the duration
// of the session and redraws cleanly on exit.
//
// SIGWINCH is unix-only, so this file is unix-only. main.go is
// already unix-only via stderr_unix.go, so we lose nothing — the
// release matrix doesn't target Windows.

package cluster

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/klog/v2"
)

// ExecCmd drives an interactive exec session against one pod
// container. It implements bubbletea's tea.ExecCommand interface
// (SetStdin / SetStdout / SetStderr / Run) so callers route it
// through tea.Exec — the program releases the terminal, we run, the
// program reclaims the terminal on return.
type ExecCmd struct {
	restCfg   *rest.Config
	namespace string
	pod       string
	container string
	command   []string

	// Filled by tea.Program.exec before Run() is called. In practice
	// stdin is the program's input (*os.File pointing at the real
	// terminal); we type-assert and refuse if it isn't, because
	// without a real fd we can't put the terminal in raw mode and
	// the interactive shell would be useless.
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

// NewExecCmd builds an ExecCmd ready to hand to tea.Exec. The actual
// SPDY connection and terminal raw-mode happen later in Run().
func (s *Supervisor) NewExecCmd(ctxName, namespace, pod, container string, command []string) (*ExecCmd, error) {
	restCfg, err := s.RestConfigFor(ctxName)
	if err != nil {
		return nil, fmt.Errorf("rest config: %w", err)
	}
	// Interactive shells are long-lived by design. The default
	// per-request timeout from probeOnce would kill the session in
	// 5s.
	restCfg.Timeout = 0
	return &ExecCmd{
		restCfg:   restCfg,
		namespace: namespace,
		pod:       pod,
		container: container,
		command:   command,
	}, nil
}

func (c *ExecCmd) SetStdin(r io.Reader)  { c.stdin = r }
func (c *ExecCmd) SetStdout(w io.Writer) { c.stdout = w }
func (c *ExecCmd) SetStderr(w io.Writer) { c.stderr = w }

// Run is called by bubbletea after it has released the terminal.
// It must block for the lifetime of the exec session.
func (c *ExecCmd) Run() error {
	stdinFile, ok := c.stdin.(*os.File)
	if !ok {
		return fmt.Errorf("exec: stdin is not a *os.File; cannot drive PTY")
	}
	fd := int(stdinFile.Fd())
	if !term.IsTerminal(fd) {
		return fmt.Errorf("exec: stdin is not a terminal (need an interactive TTY)")
	}

	clientset, err := kubernetes.NewForConfig(c.restCfg)
	if err != nil {
		return fmt.Errorf("clientset: %w", err)
	}

	// Build the pods/exec subresource request. With TTY=true the
	// apiserver merges stderr into stdout, so we leave Stderr off
	// on both the request and the StreamOptions below.
	req := clientset.CoreV1().RESTClient().Post().
		Namespace(c.namespace).
		Resource("pods").
		Name(c.pod).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: c.container,
			Command:   c.command,
			Stdin:     true,
			Stdout:    true,
			Stderr:    false,
			TTY:       true,
		}, scheme.ParameterCodec)

	// WebSocket-first with SPDY fallback. WS (v5.channel.k8s.io) is
	// the forward-looking subprotocol — kubectl moved to it for
	// k8s ≥ 1.29 because SPDY is on the long-deprecation path. The
	// fallback kicks in for older apiservers (or any proxy/ingress
	// that strips the WS upgrade), keeping kubetin usable against
	// legacy clusters without forking the code path.
	spdyExecutor, err := remotecommand.NewSPDYExecutor(c.restCfg, "POST", req.URL())
	if err != nil {
		if apierrors.IsForbidden(err) {
			return fmt.Errorf("rbac: create pods/exec denied")
		}
		return fmt.Errorf("spdy exec setup: %w", err)
	}
	wsExecutor, err := remotecommand.NewWebSocketExecutor(c.restCfg, "GET", req.URL().String())
	if err != nil {
		return fmt.Errorf("ws exec setup: %w", err)
	}
	executor, err := remotecommand.NewFallbackExecutor(wsExecutor, spdyExecutor, httpstream.IsUpgradeFailure)
	if err != nil {
		return fmt.Errorf("fallback exec setup: %w", err)
	}

	// Raw mode: byte-at-a-time stdin, no local line buffering, no
	// local echo (the remote shell echoes for us). Restoring is
	// load-bearing — if we leave the terminal in raw mode on exit,
	// the user's next shell command is unreadable. defer makes this
	// robust even when StreamWithContext panics or returns early.
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("term raw: %w", err)
	}
	defer func() {
		_ = term.Restore(fd, oldState)
	}()

	// SIGWINCH handling: the user can resize their terminal during
	// the session. We push the current size on start, then any new
	// size on each SIGWINCH. The executor reads via Next() and
	// forwards to the remote PTY.
	sizeQ := newResizeQueue(fd)
	defer sizeQ.stop()

	// Enter our own alt-screen buffer for the duration of the exec
	// session. Without this, bubbletea's tea.Exec has already exited
	// its alt-screen by the time we get here, which exposes whatever
	// was on the user's normal terminal buffer before kubetin
	// started — the shell prompt then renders on top of that
	// pre-kubetin content. Entering alt-screen (1049h) clears the
	// alt buffer on entry, gives us a clean canvas, and on exit
	// (1049l) restores the pre-kubetin contents the user expects to
	// see *outside* kubetin. bubbletea's RestoreTerminal() then
	// re-enters its own alt-screen for the TUI on top.
	//
	// Deferred last so it fires first on return: alt-screen exit
	// before raw-mode restore feels visually cleaner — the user
	// sees their original terminal a beat before any focus blip
	// from re-entering raw cooked mode.
	const (
		altScreenOn  = "\x1b[?1049h\x1b[H" // enter alt-screen + home cursor
		altScreenOff = "\x1b[?1049l"
	)
	if _, err := io.WriteString(c.stdout, altScreenOn); err != nil {
		return fmt.Errorf("term alt-screen on: %w", err)
	}
	defer func() {
		_, _ = io.WriteString(c.stdout, altScreenOff)
	}()

	klog.Infof("exec: starting %s/%s container=%s cmd=%v",
		c.namespace, c.pod, c.container, c.command)

	err = executor.StreamWithContext(context.Background(), remotecommand.StreamOptions{
		Stdin:             c.stdin,
		Stdout:            c.stdout,
		Stderr:            nil, // merged into Stdout in TTY mode
		Tty:               true,
		TerminalSizeQueue: sizeQ,
	})

	klog.Infof("exec: ended %s/%s container=%s err=%v",
		c.namespace, c.pod, c.container, err)
	return err
}

// resizeQueue is a remotecommand.TerminalSizeQueue driven by
// SIGWINCH. The initial size is pushed on construction; subsequent
// sizes come from a SIGWINCH-triggered goroutine. Bounded to 4: if
// the executor is slow to consume, intermediate sizes get dropped
// in favour of the latest — the remote PTY only cares about the
// current size, not the history.
type resizeQueue struct {
	fd     int
	sizes  chan remotecommand.TerminalSize
	sigs   chan os.Signal
	stopCh chan struct{}
}

func newResizeQueue(fd int) *resizeQueue {
	q := &resizeQueue{
		fd:     fd,
		sizes:  make(chan remotecommand.TerminalSize, 4),
		sigs:   make(chan os.Signal, 1),
		stopCh: make(chan struct{}),
	}
	if w, h, err := term.GetSize(fd); err == nil {
		q.push(uint16(w), uint16(h))
	}
	signal.Notify(q.sigs, syscall.SIGWINCH)
	go q.loop()
	return q
}

func (q *resizeQueue) loop() {
	for {
		select {
		case <-q.stopCh:
			return
		case <-q.sigs:
			if w, h, err := term.GetSize(q.fd); err == nil {
				q.push(uint16(w), uint16(h))
			}
		}
	}
}

func (q *resizeQueue) push(w, h uint16) {
	sz := remotecommand.TerminalSize{Width: w, Height: h}
	for {
		select {
		case q.sizes <- sz:
			return
		default:
			// Bump the oldest pending size out of the way — the
			// remote PTY only needs the latest dimensions, not the
			// trail of every intermediate resize.
			select {
			case <-q.sizes:
			default:
				return
			}
		}
	}
}

// Next blocks until a resize event arrives or the queue is stopped.
// Returning nil tells the executor "no more sizes coming" so it
// stops calling us.
func (q *resizeQueue) Next() *remotecommand.TerminalSize {
	select {
	case sz, ok := <-q.sizes:
		if !ok {
			return nil
		}
		return &sz
	case <-q.stopCh:
		return nil
	}
}

func (q *resizeQueue) stop() {
	signal.Stop(q.sigs)
	close(q.stopCh)
}
