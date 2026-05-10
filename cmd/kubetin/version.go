// Build identification.
//
// We don't ship a -ldflags=-X version constant. Go since 1.18 stamps
// the module version + VCS metadata into every binary built with
// `go build`, and `runtime/debug.ReadBuildInfo` reads it back. That
// gives us a useful version string for free in three cases:
//   - go install of a tagged module → version like "v0.3.1"
//   - go build inside the repo with VCS info → "(devel)" + commit hash
//   - go build of an exported tarball → "(devel)" only
//
// Anything more elaborate (release branch + dirty-tree marker baked in
// by CI) belongs in a future packaging step.
package main

import (
	"fmt"
	"runtime/debug"
	"strings"
)

// versionString returns a single line suitable for printing or for
// embedding in the help overlay. Never returns "" — callers can use
// it unconditionally.
func versionString() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "kubetin (unknown build)"
	}
	ver := bi.Main.Version
	if ver == "" || ver == "(devel)" {
		ver = "(devel)"
	}

	var rev, when string
	dirty := false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.time":
			when = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}

	var b strings.Builder
	b.WriteString("kubetin ")
	b.WriteString(ver)
	if rev != "" {
		short := rev
		if len(short) > 7 {
			short = short[:7]
		}
		b.WriteString(" · ")
		b.WriteString(short)
		if dirty {
			b.WriteString("+dirty")
		}
	}
	if when != "" {
		b.WriteString(" · ")
		b.WriteString(when)
	}
	return b.String()
}

// printVersion prints the version line and a Go version footer.
func printVersion() {
	bi, _ := debug.ReadBuildInfo()
	fmt.Println(versionString())
	if bi != nil {
		fmt.Printf("go: %s\n", bi.GoVersion)
	}
}
