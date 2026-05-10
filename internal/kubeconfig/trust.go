// Trust list for discovered kubeconfigs.
//
// Why this exists: a kubeconfig isn't passive data — its user[].exec
// stanza causes kubetin to spawn arbitrary processes (gke-gcloud-auth-
// plugin, aws-iam-auth, kubelogin, anything on $PATH) every time we
// open a connection to that context. Auto-discovering everything in
// ~/.kube/ means a colleague who can drop a file into that directory
// can land code execution at our next launch.
//
// The defence is small: keep a content-hash allow-list. On startup we
// hash every candidate file and only trust the ones whose hash we've
// already accepted. New or changed files are surfaced once and the
// user must re-bless them with `kubetin -trust` (or interactively on
// the very first run, when there's nothing to compare against).
//
// Hash-anchored, not path-anchored: re-trusting a known path does not
// auto-bless a *new content* with the same path — useful when a
// kubeconfig is rotated or replaced silently.
package kubeconfig

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// TrustList holds the user's kubeconfig allow-list.
type TrustList struct {
	// path is the on-disk file we round-trip. Empty means we couldn't
	// resolve a config dir; operations become no-ops.
	path string
	// hashes maps sha256(content) -> source file path at trust time.
	// We key on hash so a swapped file with the same path does not
	// inherit the old trust.
	hashes map[string]string
	// existed records whether the trust file was present on disk at
	// load time — separately from whether we could PARSE it. Lets
	// callers distinguish "first run" (file absent) from "file is
	// present but couldn't be read" (corruption, perms, etc.) and
	// fail closed on the latter instead of silently treating it as
	// first-run and overwriting on accept.
	existed bool
}

// LoadTrustList reads the trust file from $XDG_CONFIG_HOME/kubetin or
// ~/.config/kubetin. Missing file is not an error — returns an empty
// list whose Save() can write a fresh one.
func LoadTrustList() (*TrustList, error) {
	path, err := trustFilePath()
	if err != nil {
		return &TrustList{hashes: map[string]string{}}, err
	}
	tl := &TrustList{path: path, hashes: map[string]string{}}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Genuinely a first run — existed stays false.
			return tl, nil
		}
		// File is there (permissions, IO error, etc.) but we can't
		// read it. Mark it as existed so the caller knows not to
		// treat this as first-run.
		tl.existed = true
		return tl, err
	}
	tl.existed = true
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Format: "<sha256>  <path>". The path is informational — we
		// match purely on hash, but keep it for debugging.
		parts := strings.SplitN(line, "  ", 2)
		if len(parts) == 0 || len(parts[0]) != 64 {
			continue
		}
		src := ""
		if len(parts) == 2 {
			src = parts[1]
		}
		tl.hashes[parts[0]] = src
	}
	return tl, sc.Err()
}

// IsTrusted reports whether the given file's current content hash is
// in the allow-list.
func (t *TrustList) IsTrusted(path string) bool {
	h, err := hashFile(path)
	if err != nil {
		return false
	}
	_, ok := t.hashes[h]
	return ok
}

// HasKnownPath reports whether the trust list previously trusted any
// file at this path (under a different hash). Used to distinguish
// "content changed" (e.g. `oc login` rewrote the token) from "new
// file appeared" — the messages and remediation differ.
func (t *TrustList) HasKnownPath(path string) bool {
	for _, p := range t.hashes {
		if p == path {
			return true
		}
	}
	return false
}

// Add hashes the file and adds it to the in-memory list. Caller must
// Save() to persist.
func (t *TrustList) Add(path string) error {
	h, err := hashFile(path)
	if err != nil {
		return err
	}
	t.hashes[h] = path
	return nil
}

// Save writes the trust list to disk with 0o600 permissions. Atomic
// via tmp+rename so a crash mid-write can't truncate the existing list.
func (t *TrustList) Save() error {
	if t.path == "" {
		return fmt.Errorf("no trust file path resolved")
	}
	if err := os.MkdirAll(filepath.Dir(t.path), 0o700); err != nil {
		return err
	}
	tmp := t.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	fmt.Fprintln(w, "# kubetin trusted kubeconfigs — sha256 of file content + last-known path")
	fmt.Fprintln(w, "# Use `kubetin -trust` to re-bless after intentional changes.")
	keys := make([]string, 0, len(t.hashes))
	for k := range t.hashes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "%s  %s\n", k, t.hashes[k])
	}
	if err := w.Flush(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, t.path)
}

// Path returns the on-disk location of the trust file (empty if
// unresolved). Useful for error messages.
func (t *TrustList) Path() string { return t.path }

// PartitionFiles splits files into trusted vs untrusted using the
// current allow-list.
func (t *TrustList) PartitionFiles(files []string) (trusted, untrusted []string) {
	for _, f := range files {
		if t.IsTrusted(f) {
			trusted = append(trusted, f)
		} else {
			untrusted = append(untrusted, f)
		}
	}
	return
}

// IsEmpty reports whether the trust list contains zero entries.
func (t *TrustList) IsEmpty() bool { return len(t.hashes) == 0 }

// Existed reports whether the trust file was present on disk when we
// loaded — orthogonal to whether we could parse it. Callers use this
// to distinguish "first-ever run" from "file exists but corrupted";
// the latter must NOT be treated as first-run because accepting an
// interactive bless would overwrite the original list.
func (t *TrustList) Existed() bool { return t.existed }

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func trustFilePath() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "kubetin", "trusted-kubeconfigs"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "kubetin", "trusted-kubeconfigs"), nil
}

// CandidateFilesForTrust returns the same set Discover() would scan
// (KUBECONFIG override or ~/.kube/config*) without parsing them. Used
// by the trust prompt before any informer/exec stanza is touched.
func CandidateFilesForTrust() ([]string, error) {
	return candidateFiles()
}
