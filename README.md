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
- **Live tables** for pods, deployments, nodes, and events — sortable,
  filterable in-place.
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

## Run

```sh
kubetin
```

First launch prompts you to bless the kubeconfig files it discovers — see
[Trust list](#trust-list) below.

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
| View | `F1` | fleet overview |
| | `1` – `4` | pods / deployments / nodes / events |
| Filter | `/` | filter by name / namespace |
| | `n` | namespace picker |
| | `0` | all namespaces |
| | `Esc` | clear filter / namespace |
| Sort | `s` / `S` | cycle column / reverse direction |
| Inspect | `Enter` | action menu (Describe / Logs / Delete) |
| | `d` | describe selected resource |
| | `Shift-Y` | (inside Secret describe) reveal data |
| Logs | `/`, `n` / `N`, `f`, `g` / `G` | search, next/prev match, follow toggle, top/bottom |
| System | `?` | help overlay |
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
