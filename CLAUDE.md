# kubetin

Multi-cluster Kubernetes terminal monitor. bubbletea + lipgloss + client-go. Single static binary; no runtime deps beyond a kubeconfig.

## Layout

```
cmd/kubetin/        main, watch coordinator, log forwarder, trust prompt, version, fd-2 silencer
internal/cluster/   supervisor, probe, watchers (pod/node/deploy/event), focusedmetrics, networkpoll, logstream, describe, mutate (Scale/Rollout), auth (CanI/Delete)
internal/kubeconfig/  per-file discovery + content-hash trust list
internal/model/     Store with field-owning Apply methods (ProbeFields / MetricsFields)
internal/ui/        bubbletea Model/Update/View, modals, sort, filter, sidebar, header, fleet overview, log viewer
```

## Build / run

`go build -o bin/kubetin ./cmd/kubetin` — always build to `bin/kubetin`. The user runs `bin/kubetin`; building only with `go build ./...` verifies compilation but doesn't refresh the binary they're running. This bit us twice.

`go test ./...` — full suite, ~1s. Layout test in `internal/ui/layout_test.go` asserts `View()` output is exactly `m.height × m.width` cells across every (view × dimension × overlay) combination. New views/modals must be added there.

## Conventions

- Default to no comments. Add one only when the WHY is non-obvious — a hidden constraint, a workaround for a real bug, or a subtle invariant. Don't restate what the code says.
- One thing at a time. Bug fixes don't need surrounding cleanup; one-shot operations don't need helpers; three similar lines beat a premature abstraction.
- Trust internal code. Validate at boundaries (user input, external APIs); not between our own functions.
- Don't add features beyond what the task requires.

## Layout invariants (non-obvious)

The View pipeline guarantees every render is exactly `(m.width, m.height)` cells. Three helpers in `internal/ui/render.go` do the work:

- **`clampCanvas(s, w, h)`** — pad/truncate any string to exactly w×h. Body, footer, and each header line route through this in `View()`. Without it, trailing `\n` from row loops or wider-than-inner separators inside overlay boxes silently overflow and scroll the top header off-screen.
- **`padCol` / `padColRight`** — for **plain** input. Apply a single style after sizing. Use these in body row rendering.
- **`padCellANSI` / `padCellANSIRight`** — for **already-ANSI-styled** input. Use these in mixed-style cells (e.g. header label + colored sort arrow). Plain `padCol` would hand the styled string to byte-level `truncate()`, which slices through escape sequences and the broken codes bleed across the whole UI.

If you see yourself calling `style.Render(...)` *before* a padding helper, you need the ANSI-aware variant. If the padding helper applies the style, plain text is fine.

`truncate()` measures **terminal cells** (via go-runewidth, the same
library lipgloss measures with) and adds an "…" trailer when it cuts.
Not runes: a CJK ideograph or emoji occupies two columns, so a
rune-counting truncate let a 44-rune string claim 88 cells — `padCol`
returned 39 cells when asked for 20, and inside a lipgloss box that
wraps rather than clips, which breaks every fixed-height layout
downstream. Anything sourced from the cluster (event messages and
reasons, OpenShift display names, ingress hosts) can be arbitrary
Unicode.

## Async messages: per-cluster identity

Every `tea.Msg` produced asynchronously by the supervisor carries an origin Context, and every UI receiver filters on it before applying:

```go
case PodEventMsg:
    if msg.Context != m.WatchedContext { return m, nil }
    ...
```

Without this, a stale event from a cluster the user just Tabbed away from lands on the new view's data structures (UID-keyed maps; the matching DELETE comes from the now-cancelled informer and never arrives, so the foreign UID sticks). Same shape for `NodeEvent`, `DeployEvent`, `EvtEvent`, `MetricsSnapshot`, `NetworkSnapshot`, `DescribeResult`, `DeleteResult`, `ScaleResult`, `RolloutResult`.

Log streams use a session ID instead of context (multiple streams to the same context are valid). `startLogs` increments `m.logs.session`, every emitted log message is tagged with it, the receiver drops mismatches.

`PermissionResultMsg` is correct by a different shape: its cache key already encodes `ctxName`, so cross-cluster results land in different buckets.

## Per-cluster Store mutation

`model.Store` has two field-owning Apply methods:

- `ApplyProbe(ctx, ProbeFields)` — Reach, ServerVersion, NodeCount, NodeReady, LastError, AllocCPU/Mem, …
- `ApplyMetrics(ctx, MetricsFields)` — UsageCPUMilli, UsageMemBytes, MetricsAvailable, MetricsAt

Both lock the store internally and merge into the existing slot. Probe and metrics never lose each other's writes — historically `probeOnce` did Get → 10s of API calls → Set, clobbering metrics writes that arrived in between.

Don't add `Set`-style "replace whole state" callers. Use Apply methods for mutation.

## Kubeconfig handling

`internal/kubeconfig/discover.go` loads each kubeconfig file individually with `clientcmd.LoadFromFile`. We deliberately do **not** merge files via `clientcmd.Precedence` because RKE2 ships kubeconfigs whose auth user is named `default` — merging eight of them silently mis-credentials seven contexts.

`Discover()` returns one `ContextRef` per (file, context-name) pair, with two-pass disambiguation: tentative `name (basename)` suffix; if those still collide, promote to `name (parent/basename)`.

## Trust list

`$XDG_CONFIG_HOME/kubetin/trusted-kubeconfigs` is a sha256(content) allow-list. Runtime contract:

- File absent → first run, interactive prompt.
- File present and parseable → only listed files are loaded; new/changed files are surfaced and refused with a "run `kubetin -trust`" message.
- File present but **unreadable** (perms, IO error, truncation) → fail closed with an error pointing at the path. Critically, we do NOT fall back to the first-run path because Save would `O_TRUNC` over the original list.

`TrustList.Existed()` is the orthogonal-to-readable signal that distinguishes these.

## Auth (RBAC gating)

`SelfSubjectAccessReview` via `cluster.CanI`. Action menu hides actions whose SSAR returned `Allowed=false` or hasn't returned yet. Combined-form Resource arguments like `pods/log` and `deployments/scale` are split into `Resource` + `Subresource` inside `CanI` — built-in RBAC accepts the combined form but webhook/OPA authorizers don't.

## Stderr / debug log

Process startup runs `silenceStderr()` (cmd/kubetin/stderr_unix.go): `dup(2)` saved, `dup2(/dev/null, 2)`. Stops exec credential plugins (`gke-gcloud-auth-plugin`, `aws-iam-auth`, `kubelogin`) from tearing through the alt-screen with their startup chatter. The saved fd is restored before any panic-recover printf and on TUI errors.

`debug.log` lives at `$XDG_STATE_HOME/kubetin/debug.log` (mode 0o600). klog goes there. Audit-style breadcrumbs for delete / scale / rollout / secret reveal go there too.

## Things that bit us — keep in mind

- **Stale `bin/kubetin`** — `go build ./...` does not update `bin/kubetin`. Always build with `-o bin/kubetin`.
- **Sidebar separator** — `strings.Repeat("│\n", h)` produces `h+1` rows; the trailing `\n` becomes a phantom blank row that JoinVertical treats as content. The `clampCanvas` body wrap covers this now but be aware.
- **Sort arrow color** — `Theme.Title.Render("▲")` then concat onto a `Theme.Header.Render(label)` is the pattern for mixed-style headers. Pass through `padCellANSI`, never `padCol`.
- **`return m, m.cycleFocus(+1)`** — Go spec doesn't guarantee left-to-right evaluation of non-call operands. Assign `cmd := m.cycleFocus(+1); return m, cmd` for any pattern where the method mutates `m`.
- **Raw log bytes** — pods colour their output (smartmetserver does), and
  nothing stops one emitting a cursor move or an OSC title set. `applyLogLines`
  runs every line through `sanitizeLogLine` (text + `CSI … m` only); both
  renderers cut with `fitLogLine`, which never slices an escape and closes any
  colour the line left open. Feeding a coloured line to `truncate` counted the
  escape bytes as visible cells, dropped the closing `ESC[39m`, and the colour
  bled over the whole UI — surviving Esc out of the viewer.
- **Footer height varies** — `lipgloss.Height(footer)` returns 2 when filter is focused or has content, 1 otherwise. `bodyHeight` math depends on this.

## What NOT to do

- Don't add `Set`-style replace-whole-state writers to `model.Store`. Use Apply methods.
- Don't pass ANSI-styled strings into byte-level helpers (`padCol`, `truncate`).
- Don't render log lines with `truncate`; they carry producer ANSI. Use `fitLogLine`.
- Don't merge kubeconfigs through `clientcmd.Precedence`. Per-file load only.
- Don't add async receivers without a Context guard at the top.
- Don't log to `os.Stderr` from goroutines — fd 2 is `/dev/null` in TUI mode. Use `klog`.
- Don't add new keybindings without updating `internal/ui/help.go`.
