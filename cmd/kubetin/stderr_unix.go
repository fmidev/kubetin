// Plugin-stderr containment.
//
// Background: client-go's exec credential plugins inherit
// `cmd.Stderr`, which exec.Command defaults to os.Stderr. If we
// don't intervene, every gke-gcloud-auth-plugin / aws-iam-auth /
// kubelogin invocation prints its diagnostics through whatever Go's
// stderr happens to point at. Earlier code redirected os.Stderr to
// the debug.log file, which "fixed" alt-screen corruption but had
// the side-effect of writing plugin output to a 0o644 file (now
// 0o600 — but still on-disk, audit-trail-pollution).
//
// What we do here: at process start, dup the original stderr to a
// saved fd, then dup2 /dev/null over fd 2. After this:
//   - All writes via Go's os.Stderr (and child processes' inherited
//     fd 2) go to /dev/null.
//   - klog still writes to debug.log via klog.SetOutput, unaffected.
//   - Panic recover can restore the original fd via the closure we
//     return, so crash stacks still reach the user's terminal.
//
// This costs us the ability to log unrecovered panics anywhere
// useful (they go to /dev/null until our recover restores). That's
// acceptable: the recover handler in runTUI catches them. For
// production use we'd add a separate crash-log file, but that's
// outside this minimum.
package main

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// silenceStderr replaces fd 2 with /dev/null and returns a function
// that restores the original stderr. Errors are non-fatal — if the
// platform misbehaves we just don't get the protection.
//
// We use unix.Dup2 from golang.org/x/sys/unix instead of syscall.Dup2
// because syscall.Dup2 isn't provided on linux/arm64 (and other
// newer Linux architectures) — those archs only expose dup3(2), and
// the stdlib syscall package never wrapped it. unix.Dup2 abstracts
// over this: real Dup2 where it exists, Dup3(…, 0) where it doesn't.
// syscall.Dup and syscall.Close still work cross-arch.
func silenceStderr() (restore func(), err error) {
	saved, err := syscall.Dup(2)
	if err != nil {
		return func() {}, err
	}
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		_ = syscall.Close(saved)
		return func() {}, err
	}
	if err := unix.Dup2(int(null.Fd()), 2); err != nil {
		_ = null.Close()
		_ = syscall.Close(saved)
		return func() {}, err
	}
	_ = null.Close() // dup2 keeps it open via fd 2

	return func() {
		_ = unix.Dup2(saved, 2)
		_ = syscall.Close(saved)
	}, nil
}
