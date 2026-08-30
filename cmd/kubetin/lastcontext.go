package main

import (
	"os"
	"path/filepath"
	"strings"

	"k8s.io/klog/v2"
)

// lastContextFile remembers the cluster the user was last looking at so
// the next launch opens there instead of on whichever context probed
// healthy first. It lives beside debug.log in the state dir: this is
// session scratch, not configuration the user edits.
const lastContextFile = "last-context"

// stateDir returns $XDG_STATE_HOME/kubetin, creating it.
func stateDir() (string, error) {
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		state = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(state, "kubetin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// loadLastContext returns the remembered context name, or "" if there
// is none. Every failure is a "" — a missing or corrupt state file must
// never keep kubetin from starting.
func loadLastContext() string {
	dir, err := stateDir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(dir, lastContextFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// saveLastContext records ctx as the context to reopen on. Best effort:
// a failure here costs the user one restart on the wrong cluster, so it
// goes to debug.log rather than the UI.
func saveLastContext(ctx string) {
	dir, err := stateDir()
	if err != nil {
		klog.Warningf("last-context: %v", err)
		return
	}
	// 0o600 to match debug.log: context names carry cluster and often
	// customer names, and workstations are shared.
	if err := os.WriteFile(filepath.Join(dir, lastContextFile), []byte(ctx+"\n"), 0o600); err != nil {
		klog.Warningf("last-context: %v", err)
	}
}
