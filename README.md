# kubetin

Multi-cluster Kubernetes terminal monitor.

> *kubetin*: from Finnish *vekotin* — "a useful little gadget."

A `top`-style TUI for keeping an eye on a fleet of Kubernetes clusters at
once. Built on bubbletea + lipgloss + client-go, ships as a single static
binary, needs nothing at runtime beyond your kubeconfig — no daemon, no
agent, no installed CRDs.

## What it does

- **Multi-cluster fleet view.** Every context in your kubeconfig is probed
  in the background; switch focus with Tab.
- **Live tables** for pods, deployments, nodes, events, namespaces,
  services and ingresses — sortable, filterable in-place.
- **Endpoint health on services.** The `READY` column counts ready
  endpoints across every EndpointSlice, so a Service whose selector
  matches nothing reads `0/0` in red instead of looking healthy.
- **Resource metrics** (CPU / memory) from metrics-server, when present.
- **Per-pod network rates** scraped from kubelet/cAdvisor through the
  apiserver proxy. Hidden gracefully when RBAC denies `nodes/proxy`.
- **Log streaming** with auto-reconnect on stream drops.
- **Inline mutations**: describe, scale, rollout-restart, delete — each
  gated by a `SelfSubjectAccessReview` so the UI hides actions you can't
  perform.
- **OpenShift- and tenant-aware.** Watchers self-resolve the right scope
  (cluster-wide vs. namespace) by probing actual access, not just the
  kubeconfig hint — so a microk8s admin sees everything, an OpenShift
  project user sees their project.

## Install

Requires Go 1.26+.

```sh
go install github.com/fmidev/kubetin/cmd/kubetin@latest
```

Or build from source:

```sh
git clone https://github.com/fmidev/kubetin
cd kubetin
go build -o bin/kubetin ./cmd/kubetin
```

Or grab a prebuilt binary from
[Releases](https://github.com/fmidev/kubetin/releases):

```sh
VERSION=v1.6.0
ARCH=darwin-arm64   # or linux-amd64, linux-arm64

curl -fsSLO https://github.com/fmidev/kubetin/releases/download/$VERSION/kubetin-$VERSION-$ARCH.tar.gz
tar -xzf kubetin-$VERSION-$ARCH.tar.gz
sudo install -m 755 kubetin-$VERSION-$ARCH/kubetin /usr/local/bin/kubetin
```

### macOS: "Apple could not verify kubetin is free of malware"

Release binaries are not notarized — that needs a paid Apple Developer
account, which this project doesn't have. Nothing is wrong with the
binary: the Go linker ad-hoc signs it, so it's intact and runnable.
macOS refuses it because it arrived carrying a *quarantine* flag and
Apple has never seen it. `spctl -a` reports `rejected` for the same
reason.

The `curl` + `tar` route above never sets that flag, so the prompt
doesn't appear at all. It shows up when you download through a browser
and unpack with Finder, which copies the flag onto the extracted files.

If you've already hit it, clear the flag:

```sh
xattr -d com.apple.quarantine /usr/local/bin/kubetin
```

Or allow it through **System Settings → Privacy & Security**, where a
"kubetin was blocked" row with an **Open Anyway** button appears
immediately after a blocked launch attempt.

`go install` compiles on your own machine, so nothing is ever
quarantined and none of this applies.

## Run

```sh
kubetin
```

First launch prompts you to bless the kubeconfig files it discovers — see
[Trust list](#trust-list) below.

The cluster rail on the left is hidden automatically when your kubeconfig
holds a single context — the header already carries that cluster's name,
version, node counts and resource bars, so the rail would only cost the
table 30 columns. Toggle it any time with `C`, or start without it:

```sh
kubetin -no-sidebar
```

Press `?` once running for the full keybinding list.

## Trust list

`kubetin` runs whatever exec-credential plugins are referenced from your
kubeconfig (`gke-gcloud-auth-plugin`, `aws-iam-auth`, `kubelogin`, …). To
keep a tampered kubeconfig from silently swapping in a different binary,
kubetin maintains a sha256 allow-list at
`$XDG_CONFIG_HOME/kubetin/trusted-kubeconfigs`.

Runtime contract:

- File absent → first run, interactive prompt.
- File present, parseable → only listed files are loaded; new or modified
  files are surfaced and refused with a `kubetin -trust` hint.
- File present but unreadable → fail closed (we never overwrite an
  existing list with a fresh "first-run" save).

After `oc login`, `gcloud auth login`, or any other rewrite of a tracked
kubeconfig, the file's hash changes. Re-bless with:

```sh
kubetin -trust
```

## Keybindings

| Group | Key | Action |
|---|---|---|
| Move | `j` / `↓` | next row |
| | `k` / `↑` | previous row |
| | `g` / `G` | first / last row |
| Cluster | `Tab` / `Shift-Tab` | next / previous reachable cluster |
| | `C` | show / hide the cluster rail |
| View | `F1` | fleet overview |
| | `1` – `6` | pods / deployments / services / ingresses / nodes / namespaces |
| Inspect the selected row | `i` | status dashboard |
| | `l` | logs |
| | `e` | events |
| | `d` | describe |
| | `Enter` | action menu |
| Filter | `/` | filter by name / namespace |
| | `n` | namespace picker |
| | `0` | all namespaces |
| | `Esc` | clear filter / namespace |
| Sort | `s` / `S` | cycle column / reverse direction |
| Events lens | `E` | open for the namespace (or cluster, with `ns: all`) |
| | `E` | (inside) widen to everything |
| | `e` / `Esc` | (inside) close |
| Logs viewer | `/`, `n` / `N`, `f`, `g` / `G` | search, next/prev match, follow toggle, top/bottom |
| System | `?` | help overlay (scrolls with `j` / `k`) |
| | `Shift-Y` | reveal Secret data inside describe |
| | `F2` | debug overlay |
| | `q` / `Ctrl-C` | quit |

## Files

| Path | Purpose |
|---|---|
| `$XDG_CONFIG_HOME/kubetin/trusted-kubeconfigs` | sha256 allow-list of trusted kubeconfig files |
| `$XDG_STATE_HOME/kubetin/debug.log` | klog output and audit breadcrumbs (mode `0600`) |

## Layout

```
cmd/kubetin/         main, watch coordinator, log forwarder, trust prompt
internal/cluster/    supervisor, probe, watchers, metrics, network, logs,
                     describe, mutate (Scale / Rollout / Delete), auth (CanI)
internal/kubeconfig/ per-file discovery and content-hash trust list
internal/model/      thread-safe Store with field-owning Apply methods
internal/ui/         bubbletea Model / Update / View, modals, sort, filter
```

## Releases

Driven by [release-please](https://github.com/googleapis/release-please).
Use conventional-commit subjects on PR merges:

```
feat: add fleet-overview heat-map
fix: stop crashing on empty kubeconfig
chore: bump client-go to v0.32
```

The bot opens a `chore(main): release X.Y.Z` PR that accumulates the
changelog. Merging it tags + creates the GitHub Release; CI then builds
darwin-arm64 / linux-amd64 / linux-arm64 binaries and attaches them.

## License

MIT — see [LICENSE](LICENSE).
