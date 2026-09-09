package kubeconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func writeConfig(t *testing.T, file, server string, names ...string) {
	t.Helper()
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["default"] = &clientcmdapi.Cluster{Server: server}
	cfg.AuthInfos["default"] = &clientcmdapi.AuthInfo{Token: server}
	for _, name := range names {
		cfg.Contexts[name] = &clientcmdapi.Context{Cluster: "default", AuthInfo: "default", Namespace: "team"}
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := clientcmd.WriteToFile(*cfg, file); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoveryDisambiguatesSources(t *testing.T) {
	for _, tc := range []struct {
		name  string
		paths []string
		raw   []string
		want  []string
	}{
		{"unique", []string{"a/config", "b/config"}, []string{"prod", "test"}, []string{"prod", "test"}},
		{"basenames", []string{"config-a", "config-b"}, []string{"prod", "prod"}, []string{"prod (config-a)", "prod (config-b)"}},
		{"parents", []string{"a/config", "b/config"}, []string{"prod", "prod"}, []string{"prod (a/config)", "prod (b/config)"}},
		{"kube-directories", []string{"a/.kube/config", "b/.kube/config"}, []string{"prod", "prod"}, []string{"prod (a/.kube/config)", "prod (b/.kube/config)"}},
		{"deep-tails", []string{"a/shared/shared/.kube/config", "b/shared/shared/.kube/config"}, []string{"prod", "prod"}, []string{"prod (a/shared/shared/.kube/config)", "prod (b/shared/shared/.kube/config)"}},
		{"literal-label", []string{"a/config", "b/config", "c/config"}, []string{"prod", "prod", "prod (a/config)"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			var files []string
			tl := &TrustList{hashes: make(map[string]string)}
			for i, path := range tc.paths {
				file := filepath.Join(dir, path)
				files = append(files, file)
				writeConfig(t, file, fmt.Sprintf("https://server-%d.invalid", i), tc.raw[i])
				if err := tl.Add(file); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("KUBECONFIG", strings.Join(files, string(os.PathListSeparator)))
			d, err := Discover()
			if err != nil {
				t.Fatal(err)
			}
			trusted, untrusted, err := DiscoverTrusted(tl)
			if err != nil || len(untrusted) != 0 || !reflect.DeepEqual(d, trusted) {
				t.Fatalf("trusted discovery differs: %v, untrusted=%v", err, untrusted)
			}
			assertUniqueRefs(t, d, len(files))
			if tc.want != nil && !reflect.DeepEqual(d.Contexts, tc.want) {
				t.Fatalf("names = %v, want %v", d.Contexts, tc.want)
			}
			for _, r := range d.Refs {
				i := slices.Index(files, r.File)
				if r.RawName != tc.raw[i] || r.Namespace != "team" || d.Configs[r.File].Clusters["default"].Server != fmt.Sprintf("https://server-%d.invalid", i) {
					t.Fatalf("source was changed: %+v", r)
				}
			}
			// Repeated entries and environment order must not change identities or labels.
			slices.Reverse(files)
			files = append(files, files[0])
			t.Setenv("KUBECONFIG", strings.Join(files, string(os.PathListSeparator)))
			reordered, err := Discover()
			if err != nil || !reflect.DeepEqual(d, reordered) {
				t.Fatalf("discovery depends on path order or repetition: %v", err)
			}
		})
	}
}

func assertUniqueRefs(t *testing.T, d *Discovered, count int) {
	t.Helper()
	if len(d.Refs) != count || len(d.Contexts) != count {
		t.Fatalf("got %d refs and %d names, want %d", len(d.Refs), len(d.Contexts), count)
	}
	names, ids := make(map[string]bool), make(map[ContextID]bool)
	for _, r := range d.Refs {
		if names[r.Name] || ids[r.ID()] || !filepath.IsAbs(r.File) {
			t.Fatalf("duplicate or non-absolute source: %+v", r)
		}
		names[r.Name], ids[r.ID()] = true, true
		if found, ok := d.RefByName(r.Name); !ok || found != r {
			t.Fatalf("label resolves to a different source: %+v", r)
		}
	}
}

func TestDiscoveryReservesEvenFullPathLabels(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a/config"), filepath.Join(dir, "b/config")
	reserved := []string{"prod"}
	for _, tail := range pathTails(a) {
		reserved = append(reserved, "prod ("+tail+")")
	}
	reserved = append(reserved, "prod ("+a+") [2]")
	writeConfig(t, a, "https://a.invalid", "prod")
	writeConfig(t, b, "https://b.invalid", reserved...)
	t.Setenv("KUBECONFIG", a+string(os.PathListSeparator)+b)
	d, err := Discover()
	if err != nil {
		t.Fatal(err)
	}
	assertUniqueRefs(t, d, len(reserved)+1)
	for _, name := range reserved[1:] {
		if r, ok := d.RefByName(name); !ok || r.RawName != name || r.File != b {
			t.Fatalf("generated name took literal context %q", name)
		}
	}
	r, ok := d.RefByName("prod (" + a + ") [3]")
	if !ok || r.File != a || r.RawName != "prod" {
		t.Fatalf("full-path collision was not resolved: %+v", d.Refs)
	}
}

func TestTrustedDiscoveryKeepsIdentityWhenLabelsChange(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a/.kube/config"), filepath.Join(dir, "b/.kube/config")
	writeConfig(t, a, "https://a.invalid", "prod")
	writeConfig(t, b, "https://b.invalid", "prod")
	t.Setenv("KUBECONFIG", a+string(os.PathListSeparator)+b)
	tl := &TrustList{hashes: make(map[string]string)}
	if err := tl.Add(a); err != nil {
		t.Fatal(err)
	}
	first, untrusted, err := DiscoverTrusted(tl)
	if err != nil || !reflect.DeepEqual(untrusted, []string{b}) || len(first.Configs) != 1 {
		t.Fatalf("untrusted source was loaded: %v, %v", err, untrusted)
	}
	if err := tl.Add(b); err != nil {
		t.Fatal(err)
	}
	second, _, err := DiscoverTrusted(tl)
	if err != nil {
		t.Fatal(err)
	}
	if first.Refs[0].ID() != second.Refs[0].ID() || first.Refs[0].Name == second.Refs[0].Name {
		t.Fatal("identity changed along with the display label")
	}
}

func TestDiscoveryDeduplicatesRelativeAndAbsolutePaths(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config")
	writeConfig(t, file, "https://a.invalid", "prod")
	t.Chdir(dir)
	t.Setenv("KUBECONFIG", strings.Join([]string{"config", "./config", file}, string(os.PathListSeparator)))
	d, err := Discover()
	if err != nil {
		t.Fatal(err)
	}
	assertUniqueRefs(t, d, 1)
	if len(d.Files) != 1 || d.Files[0] != file || d.Contexts[0] != "prod" {
		t.Fatalf("path aliases created duplicate contexts: %+v", d)
	}
}
