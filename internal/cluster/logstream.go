package cluster

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"maps"
	"strings"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// LogLine is a single record from a log stream. Exactly one of the
// content fields is meaningful per record:
//   - Line: a log line (timestamp prefix preserved)
//   - Err:  fatal stream error; the streamer is giving up
//   - EOS:  end-of-stream; sent exactly once on shutdown
//   - Reconnecting: transient hiccup; the streamer is about to retry.
//     Err on the same record carries the cause (informational only —
//     UI shouldn't show it as fatal because we'll reattempt).
type LogLine struct {
	Line         string
	Err          string
	EOS          bool
	Reconnecting bool
}

// LogStreamer streams pod logs over a buffered channel. Cancel via
// the parent context to stop the underlying HTTP stream.
type LogStreamer struct {
	Context   string
	Namespace string
	Pod       string
	Container string
	Out       chan LogLine
	Dropped   atomic.Uint64
}

// maxConsecutiveStreamFailures bounds reconnect attempts. After this
// many failures with no new timestamped line read in between, the streamer
// gives up and emits a fatal LogLine{Err}. Five retries add 15s of
// backoff, excluding API request time.
const maxConsecutiveStreamFailures = 5

// StreamLogs starts a follow=true log stream against pod/container,
// auto-reconnecting on transient errors (TCP reset, EOF, idle timeout).
// tailLines bounds the historical chunk replayed before live tailing
// on the FIRST attempt; reconnects use SinceTime instead so we resume
// from the last seen log timestamp without replaying history.
//
// The initial Stream call is made here on the caller's goroutine so
// fatal errors (auth, not found, kubeconfig) surface synchronously.
// Once that handshake succeeds, reading and reconnecting all happen
// on the background goroutine.
func (s *Supervisor) StreamLogs(parent context.Context, ctxName, namespace, pod, container string, tailLines int) (*LogStreamer, error) {
	restCfg, err := s.RestConfigFor(ctxName)
	if err != nil {
		return nil, fmt.Errorf("rest config: %w", err)
	}
	// No HTTP timeout — log streams are long-running by design.

	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("clientset: %w", err)
	}

	// First attempt synchronous to surface auth/not-found errors.
	// A negative tailLines means no limit — the whole log, the way
	// `kubectl logs` behaves. openStream reads a nil tail as "no
	// TailLines option", which is exactly that.
	var tailPtr *int64
	if tailLines >= 0 {
		tail := int64(tailLines)
		tailPtr = &tail
	}
	stream, err := openStream(parent, clientset, namespace, pod, container, tailPtr, time.Time{})
	if err != nil {
		return nil, err
	}

	out := make(chan LogLine, 256)
	ls := &LogStreamer{
		Context:   ctxName,
		Namespace: namespace,
		Pod:       pod,
		Container: container,
		Out:       out,
	}

	go runStream(parent, clientset, ls, stream, namespace, pod, container, tailLines)

	return ls, nil
}

// runStream drains the open stream and reopens on transient failures.
// State carried across reconnects is the last-seen log timestamp,
// extracted from kubelet's `Timestamps: true` line prefix.
func runStream(parent context.Context, cs *kubernetes.Clientset, ls *LogStreamer, initial io.ReadCloser, namespace, pod, container string, tailLines int) {
	defer close(ls.Out)

	klog.Infof("logstream[%s/%s/%s/%s]: opened (tail=%d)",
		ls.Context, namespace, pod, container, tailLines)

	stream := initial
	finishing := false
	cursor := logCursor{}
	consec := 0

	defer func() {
		if stream != nil {
			_ = stream.Close()
		}
		// EOS marker. Blocking so the UI's "ended" indicator is
		// guaranteed to fire even on a full Out buffer.
		ls.sendBlocking(parent, LogLine{EOS: true})
		klog.Infof("logstream[%s/%s/%s]: closed", ls.Context, namespace, pod)
	}()

	for {
		cursor.beginStream()
		// Watchdog: close the active stream if parent cancels mid-read.
		// Per-loop watchdog rather than one-shot so a reconnected
		// stream is also covered.
		streamRef := stream
		watchDone := make(chan struct{})
		go func() {
			select {
			case <-parent.Done():
				if streamRef != nil {
					_ = streamRef.Close()
				}
			case <-watchDone:
			}
		}()

		gotLine := false
		var scanErr error
		if stream != nil {
			sc := bufio.NewScanner(stream)
			sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			for sc.Scan() {
				if parent.Err() != nil {
					break
				}
				line := sc.Text()
				if t, ok := parseLogTimestamp(line); ok {
					if !cursor.accept(t, line) {
						continue
					}
					gotLine = true
				}
				ls.send(LogLine{Line: line})
			}
			scanErr = sc.Err()
			_ = stream.Close()
		}
		close(watchDone)

		if parent.Err() != nil {
			return
		}
		if stream != nil && scanErr == nil {
			if finishing {
				return
			}
			// Confirm completion before opening one final stream, so writes
			// between this EOF and the status response are drained as well.
			finishing = logsComplete(parent, cs, namespace, pod, container)
			if finishing {
				consec = 0
			}
		}

		if gotLine {
			consec = 0
		}
		consec++
		if consec > maxConsecutiveStreamFailures {
			cause := "stream ended"
			if scanErr != nil && scanErr != io.EOF {
				cause = scanErr.Error()
			}
			ls.sendBlocking(parent, LogLine{Err: cause})
			return
		}

		// Signal the reconnect attempt before sleeping so the UI can
		// flip its indicator promptly. Cause goes along for debug.log.
		cause := "stream ended"
		if scanErr != nil && scanErr != io.EOF {
			cause = scanErr.Error()
		}
		klog.V(2).Infof("logstream[%s/%s/%s]: reconnect %d/%d: %s",
			ls.Context, namespace, pod, consec, maxConsecutiveStreamFailures, cause)
		ls.sendBlocking(parent, LogLine{Reconnecting: true, Err: cause})

		// Linear backoff (1s, 2s, …, capped). Cancellable.
		backoff := time.Duration(consec) * time.Second
		if backoff > 5*time.Second {
			backoff = 5 * time.Second
		}
		select {
		case <-time.After(backoff):
		case <-parent.Done():
			return
		}

		// Reopen with SinceTime so we don't replay all the lines we
		// already showed. If the cursor is zero (never received a
		// timestamped line — unusual since we set Timestamps:true),
		// fall back to a 5s window.
		since := cursor.at
		if since.IsZero() {
			since = time.Now().Add(-5 * time.Second)
		}
		next, openErr := openStream(parent, cs, namespace, pod, container, nil, since)
		if openErr != nil {
			if isFatalStreamErr(openErr) {
				ls.sendBlocking(parent, LogLine{Err: openErr.Error()})
				return
			}
			// Treat as transient: another loop iteration. Stream is
			// nil; the scan loop above will skip and bump consec.
			stream = nil
			continue
		}
		stream = next
	}
}

// openStream issues the GetLogs request. tail (if non-nil) is the
// initial-attempt TailLines; on reconnect we pass nil tail and a
// non-zero since to resume.
func openStream(parent context.Context, cs *kubernetes.Clientset, namespace, pod, container string, tail *int64, since time.Time) (io.ReadCloser, error) {
	opts := &corev1.PodLogOptions{
		Container:  container,
		Follow:     true,
		Timestamps: true,
	}
	if tail != nil {
		opts.TailLines = tail
	}
	req := cs.CoreV1().Pods(namespace).GetLogs(pod, opts)
	if !since.IsZero() {
		// metav1.Time serializes query parameters at whole-second precision.
		// The API server may truncate again when forwarding to the kubelet,
		// so the reader must still discard overlap.
		req.Param("sinceTime", since.UTC().Format(time.RFC3339Nano))
	}
	stream, err := req.Stream(parent)
	if err != nil {
		return nil, fmt.Errorf("stream: %w", err)
	}
	return stream, nil
}

// The inclusive cursor retains occurrence counts at the latest timestamp.
// Counts from earlier streams consume replay without hiding additional,
// identical lines produced at that same timestamp.
type logCursor struct {
	at      time.Time
	seen    map[string]int
	overlap map[string]int
}

func (c *logCursor) beginStream() {
	c.overlap = maps.Clone(c.seen)
}

func (c *logCursor) accept(at time.Time, line string) bool {
	if at.Before(c.at) {
		return false
	}
	if c.seen == nil || at.After(c.at) {
		c.at = at
		if c.seen == nil {
			c.seen = make(map[string]int)
		} else {
			clear(c.seen)
		}
		c.overlap = nil
	}
	if c.overlap[line] > 0 {
		c.overlap[line]--
		return false
	}
	c.seen[line]++
	return true
}

func logsComplete(parent context.Context, cs *kubernetes.Clientset, namespace, pod, container string) bool {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	p, err := cs.CoreV1().Pods(namespace).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		// Logs permission need not include permission to read pod status.
		return false
	}
	return containerLogsComplete(p, container)
}

func containerLogsComplete(p *corev1.Pod, name string) bool {
	if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
		return true
	}
	// Restart rules can restart other containers too. Leave that decision
	// to the kubelet and retain bounded retries until the pod is terminal.
	for _, containers := range [][]corev1.Container{p.Spec.Containers, p.Spec.InitContainers} {
		for _, c := range containers {
			if len(c.RestartPolicyRules) > 0 {
				return false
			}
		}
	}
	for _, status := range p.Status.EphemeralContainerStatuses {
		if status.Name == name && status.State.Terminated != nil {
			return true
		}
	}
	for i, statuses := range [][]corev1.ContainerStatus{p.Status.ContainerStatuses, p.Status.InitContainerStatuses} {
		containers := p.Spec.Containers
		if i == 1 {
			containers = p.Spec.InitContainers
		}
		for _, status := range statuses {
			if status.Name != name || status.State.Terminated == nil {
				continue
			}
			for _, c := range containers {
				if c.Name != name {
					continue
				}
				policy := p.Spec.RestartPolicy
				if c.RestartPolicy != nil {
					policy = corev1.RestartPolicy(*c.RestartPolicy)
				}
				if i == 1 && c.RestartPolicy == nil && status.State.Terminated.ExitCode == 0 {
					return true
				}
				return policy == corev1.RestartPolicyNever ||
					(policy == corev1.RestartPolicyOnFailure && status.State.Terminated.ExitCode == 0)
			}
		}
	}
	return false
}

// isFatalStreamErr returns true for errors where reconnect won't help:
// auth (the user's creds aren't going to improve), not-found (the pod
// is gone or the namespace mistyped), forbidden (RBAC). Everything
// else is treated as transient.
func isFatalStreamErr(err error) bool {
	return apierrors.IsUnauthorized(err) ||
		apierrors.IsForbidden(err) ||
		apierrors.IsNotFound(err)
}

// parseLogTimestamp pulls the RFC3339Nano prefix kubelet emits when
// Timestamps:true. Lines without a parseable prefix return ok=false
// — could be malformed log content or a kubelet without timestamp
// support, in which case we fall back to a fixed time window on
// reconnect.
func parseLogTimestamp(line string) (time.Time, bool) {
	sp := strings.IndexByte(line, ' ')
	if sp <= 0 {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, line[:sp])
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func (ls *LogStreamer) send(l LogLine) {
	select {
	case ls.Out <- l:
	default:
		ls.Dropped.Add(1)
	}
}

// sendBlocking sends a control message (EOS, fatal Err) that the UI
// must observe to end the stream cleanly. Blocks until the consumer
// reads OR the parent context is done. Without this, a full Out
// buffer at shutdown would silently drop EOS and the UI would never
// receive LogEOSMsg — leaving the indicator stuck on "live".
func (ls *LogStreamer) sendBlocking(ctx context.Context, l LogLine) {
	select {
	case ls.Out <- l:
	case <-ctx.Done():
	}
}
