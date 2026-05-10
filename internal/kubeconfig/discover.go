// Package kubeconfig discovers kubeconfig files in ~/.kube and
// exposes one *clientcmdapi.Config per file. We deliberately do NOT
// merge files through clientcmd.Precedence because that silently
// collapses duplicate user/cluster names — RKE2 in particular ships
// kubeconfigs whose auth user is named "default", so merging eight
// of them produces seven mis-credentialed contexts.
package kubeconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// ContextRef describes one resolvable context: which kubeconfig file
// it lives in and the (possibly disambiguated) name we surface to the
// user.
type ContextRef struct {
	Name      string // unique across the discovery; may include " (file)" suffix
	RawName   string // name as it appears in the source kubeconfig
	File      string // absolute path to the source file
	Namespace string // context's default namespace; empty = cluster-scoped access
}

// GetName, GetRawName, GetFile, GetNamespace satisfy cluster.refLike
// so the supervisor can consume ContextRef without an import cycle.
func (r ContextRef) GetName() string      { return r.Name }
func (r ContextRef) GetRawName() string   { return r.RawName }
func (r ContextRef) GetFile() string      { return r.File }
func (r ContextRef) GetNamespace() string { return r.Namespace }

// Discovered is the result of scanning ~/.kube.
type Discovered struct {
	Files    []string
	Refs     []ContextRef
	Configs  map[string]*clientcmdapi.Config // file path -> parsed config
	Contexts []string                        // sorted list of Refs[*].Name
}

// Discover scans for kubeconfig files. It honours $KUBECONFIG when set
// (colon-separated) and otherwise globs ~/.kube/config*, filtering out
// known non-config entries.
//
// Discover does NOT consult the trust list — it returns everything
// found so callers can show the user what was scanned. Use
// DiscoverTrusted in production paths to get only allow-listed files.
func Discover() (*Discovered, error) {
	files, err := candidateFiles()
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no kubeconfig files found")
	}

	configs := make(map[string]*clientcmdapi.Config, len(files))
	var refs []ContextRef
	seen := make(map[string]int) // RawName -> count, to disambiguate

	for _, f := range files {
		cfg, err := clientcmd.LoadFromFile(f)
		if err != nil {
			// Skip unreadable files but don't fail the whole scan.
			continue
		}
		configs[f] = cfg
		for ctxName, kctx := range cfg.Contexts {
			seen[ctxName]++
			ns := ""
			if kctx != nil {
				ns = kctx.Namespace
			}
			refs = append(refs, ContextRef{
				Name:      ctxName, // patched below if duplicated
				RawName:   ctxName,
				File:      f,
				Namespace: ns,
			})
		}
	}

	// Disambiguate any context name that appears in more than one file
	// by suffixing the file's basename. Stable: sort first by file path
	// so the chosen "primary" is deterministic.
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].RawName != refs[j].RawName {
			return refs[i].RawName < refs[j].RawName
		}
		return refs[i].File < refs[j].File
	})
	// First pass: tentative basename suffix. Second pass: if those
	// suffixes still collide (two files in different directories both
	// named "config" with identically-named contexts), promote to a
	// path-tail suffix so the final names stay unique. Without the
	// second pass two contexts share a Name and the second silently
	// overwrites the first in the supervisor's per-context map.
	for i := range refs {
		if seen[refs[i].RawName] > 1 {
			refs[i].Name = refs[i].RawName + " (" + filepath.Base(refs[i].File) + ")"
		}
	}
	final := map[string]int{}
	for _, r := range refs {
		final[r.Name]++
	}
	for i := range refs {
		if final[refs[i].Name] > 1 {
			parent := filepath.Base(filepath.Dir(refs[i].File))
			refs[i].Name = refs[i].RawName + " (" + parent + "/" + filepath.Base(refs[i].File) + ")"
		}
	}

	names := make([]string, 0, len(refs))
	for _, r := range refs {
		names = append(names, r.Name)
	}
	sort.Strings(names)

	return &Discovered{
		Files:    files,
		Refs:     refs,
		Configs:  configs,
		Contexts: names,
	}, nil
}

// DiscoverTrusted scans like Discover but filters out files whose
// content hash isn't in the supplied trust list. Returns the trusted
// result plus the list of untrusted file paths so the caller can warn
// or prompt.
func DiscoverTrusted(tl *TrustList) (d *Discovered, untrusted []string, err error) {
	files, err := candidateFiles()
	if err != nil {
		return nil, nil, err
	}
	if len(files) == 0 {
		return nil, nil, fmt.Errorf("no kubeconfig files found")
	}
	trusted, untrusted := tl.PartitionFiles(files)
	if len(trusted) == 0 {
		// Return an empty Discovered so the caller can render its own
		// "nothing trusted" message; we don't want to fail the whole
		// startup the same way as "no files found at all".
		return &Discovered{Files: files, Configs: map[string]*clientcmdapi.Config{}}, untrusted, nil
	}

	// Re-run the rest of Discover() against the trusted subset only.
	configs := make(map[string]*clientcmdapi.Config, len(trusted))
	var refs []ContextRef
	seen := make(map[string]int)
	for _, f := range trusted {
		cfg, err := clientcmd.LoadFromFile(f)
		if err != nil {
			continue
		}
		configs[f] = cfg
		for ctxName, kctx := range cfg.Contexts {
			seen[ctxName]++
			ns := ""
			if kctx != nil {
				ns = kctx.Namespace
			}
			refs = append(refs, ContextRef{Name: ctxName, RawName: ctxName, File: f, Namespace: ns})
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].RawName != refs[j].RawName {
			return refs[i].RawName < refs[j].RawName
		}
		return refs[i].File < refs[j].File
	})
	// First pass: tentative basename suffix. Second pass: if those
	// suffixes still collide (two files in different directories both
	// named "config" with identically-named contexts), promote to a
	// path-tail suffix so the final names stay unique. Without the
	// second pass two contexts share a Name and the second silently
	// overwrites the first in the supervisor's per-context map.
	for i := range refs {
		if seen[refs[i].RawName] > 1 {
			refs[i].Name = refs[i].RawName + " (" + filepath.Base(refs[i].File) + ")"
		}
	}
	final := map[string]int{}
	for _, r := range refs {
		final[r.Name]++
	}
	for i := range refs {
		if final[refs[i].Name] > 1 {
			parent := filepath.Base(filepath.Dir(refs[i].File))
			refs[i].Name = refs[i].RawName + " (" + parent + "/" + filepath.Base(refs[i].File) + ")"
		}
	}
	names := make([]string, 0, len(refs))
	for _, r := range refs {
		names = append(names, r.Name)
	}
	sort.Strings(names)

	return &Discovered{
		Files:    trusted,
		Refs:     refs,
		Configs:  configs,
		Contexts: names,
	}, untrusted, nil
}

// RefByName returns the ContextRef whose Name matches.
func (d *Discovered) RefByName(name string) (ContextRef, bool) {
	for _, r := range d.Refs {
		if r.Name == name {
			return r, true
		}
	}
	return ContextRef{}, false
}

func candidateFiles() ([]string, error) {
	if env := os.Getenv("KUBECONFIG"); env != "" {
		parts := filepath.SplitList(env)
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p == "" {
				continue
			}
			if _, err := os.Stat(p); err == nil {
				out = append(out, p)
			}
		}
		return out, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("user home: %w", err)
	}
	dir := filepath.Join(home, ".kube")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}

	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "config") {
			continue
		}
		switch name {
		case "kubectx", "kubens":
			continue
		}
		if strings.HasSuffix(name, ".swp") || strings.HasSuffix(name, "~") {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}
	sort.Strings(files)
	return files, nil
}
