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

// ContextID identifies a source independently of its display name.
// The tuple avoids delimiter or hash collisions in user-controlled names.
type ContextID struct {
	File    string
	RawName string
}

func (r ContextRef) ID() ContextID {
	return ContextID{File: r.File, RawName: r.RawName}
}

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

	return discoverFiles(files), nil
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

	return discoverFiles(trusted), untrusted, nil
}

func discoverFiles(files []string) *Discovered {
	configs := make(map[string]*clientcmdapi.Config, len(files))
	var refs []ContextRef
	for _, f := range files {
		cfg, err := clientcmd.LoadFromFile(f)
		if err != nil {
			continue
		}
		configs[f] = cfg
		for name, ctx := range cfg.Contexts {
			ns := ""
			if ctx != nil {
				ns = ctx.Namespace
			}
			refs = append(refs, ContextRef{RawName: name, File: f, Namespace: ns})
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].RawName != refs[j].RawName {
			return refs[i].RawName < refs[j].RawName
		}
		return refs[i].File < refs[j].File
	})
	assignContextNames(refs)
	names := make([]string, 0, len(refs))
	for _, r := range refs {
		names = append(names, r.Name)
	}
	sort.Strings(names)
	return &Discovered{Files: files, Refs: refs, Configs: configs, Contexts: names}
}

// Reserve raw names before generating labels so a context literally named
// "prod (config)" cannot be mistaken for another file's disambiguated prod.
func assignContextNames(refs []ContextRef) {
	rawCounts := make(map[string]int)
	used := make(map[string]bool)
	for _, r := range refs {
		rawCounts[r.RawName]++
		used[r.RawName] = true
	}
	candidates := make([][]string, len(refs))
	counts := make(map[string]int)
	for i, r := range refs {
		if rawCounts[r.RawName] == 1 {
			continue
		}
		for _, tail := range pathTails(r.File) {
			name := r.RawName + " (" + tail + ")"
			candidates[i] = append(candidates[i], name)
			counts[name]++
		}
	}
	for i := range refs {
		r := &refs[i]
		if rawCounts[r.RawName] == 1 {
			r.Name = r.RawName
			continue
		}
		for _, name := range candidates[i] {
			if counts[name] == 1 && !used[name] {
				r.Name = name
				break
			}
		}
		if r.Name == "" {
			base := r.RawName + " (" + r.File + ")"
			for n := 2; ; n++ {
				name := fmt.Sprintf("%s [%d]", base, n)
				if !used[name] {
					r.Name = name
					break
				}
			}
		}
		used[r.Name] = true
	}
}

func pathTails(path string) []string {
	tails := []string{filepath.Base(path)}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		if parent := filepath.Dir(dir); parent == dir {
			return append(tails, path)
		}
		tails = append(tails, filepath.Join(filepath.Base(dir), tails[len(tails)-1]))
	}
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
		return absoluteFiles(out)
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
	return absoluteFiles(files)
}

// Keep the load path rather than resolving symlinks: relative credential paths
// in kubeconfigs are resolved relative to that path by client-go.
func absoluteFiles(files []string) ([]string, error) {
	seen := make(map[string]bool)
	out := make([]string, 0, len(files))
	for _, file := range files {
		path, err := filepath.Abs(file)
		if err != nil {
			return nil, fmt.Errorf("kubeconfig path %q: %w", file, err)
		}
		if !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out, nil
}
