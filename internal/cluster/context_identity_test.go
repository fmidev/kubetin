package cluster

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fmidev/kubetin/internal/kubeconfig"
	"github.com/fmidev/kubetin/internal/model"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestDiscoveredSelectionsKeepTheirSourceCredentials(t *testing.T) {
	dir := t.TempDir()
	var files []string
	for _, name := range []string{"a", "b"} {
		file := filepath.Join(dir, name, ".kube", "config")
		files = append(files, file)
		cfg := clientcmdapi.NewConfig()
		cfg.Clusters["default"] = &clientcmdapi.Cluster{Server: "https://" + name + ".invalid"}
		cfg.AuthInfos["default"] = &clientcmdapi.AuthInfo{Token: "token-" + name}
		cfg.Contexts["prod"] = &clientcmdapi.Context{Cluster: "default", AuthInfo: "default", Namespace: "team-" + name}
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := clientcmd.WriteToFile(*cfg, file); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("KUBECONFIG", strings.Join(files, string(os.PathListSeparator)))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "trust"))
	tl, err := kubeconfig.LoadTrustList()
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if err := tl.Add(file); err != nil {
			t.Fatal(err)
		}
	}
	d, untrusted, err := kubeconfig.DiscoverTrusted(tl)
	if err != nil || len(untrusted) != 0 {
		t.Fatalf("discovery failed: %v, untrusted=%v", err, untrusted)
	}
	sup, err := New(d, model.NewStore(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(sup.contexts) != 2 || len(sup.refs) != 2 {
		t.Fatal("one source overwrote another")
	}
	for _, name := range sup.contexts {
		ref, ok := d.RefByName(name)
		if !ok {
			t.Fatalf("context %q is not in discovery", name)
		}
		want := "a"
		if ref.File == files[1] {
			want = "b"
		}
		cfg, err := sup.RestConfigFor(name)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Host != "https://"+want+".invalid" || cfg.BearerToken != "token-"+want {
			t.Fatalf("selection %q resolved to another file's credentials", name)
		}
		src := sup.refs[ref.ID()]
		if src.Namespace != "team-"+want || src.File != ref.File || src.RawName != "prod" {
			t.Fatalf("selection %q lost its source identity: %+v", name, src)
		}
	}
	if _, err := sup.RestConfigFor("prod"); err == nil {
		t.Fatal("ambiguous raw name resolved to an arbitrary source")
	}
}

func TestSupervisorRejectsDuplicateDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name string
		refs []kubeconfig.ContextRef
	}{
		{"labels", []kubeconfig.ContextRef{
			{Name: "prod", RawName: "prod", File: "/a/config"},
			{Name: "prod", RawName: "prod", File: "/b/config"},
		}},
		{"identities", []kubeconfig.ContextRef{
			{Name: "prod-a", RawName: "prod", File: "/a/config"},
			{Name: "prod-b", RawName: "prod", File: "/a/config"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sup, err := New(&kubeconfig.Discovered{Refs: tc.refs}, model.NewStore(), time.Hour)
			if err == nil || sup != nil {
				t.Fatal("duplicate discovery silently overwrote a source")
			}
		})
	}
}
