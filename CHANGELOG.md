# Changelog

## [1.8.0](https://github.com/fmidev/kubetin/compare/v1.7.0...v1.8.0) (2026-08-30)


### Features

* events become a lens, not a view ([#50](https://github.com/fmidev/kubetin/issues/50)) ([b70192f](https://github.com/fmidev/kubetin/commit/b70192fbfd436177bcf3b0077464009885b3b760))
* scrollable help, and `l` for logs ([#51](https://github.com/fmidev/kubetin/issues/51)) ([e723185](https://github.com/fmidev/kubetin/commit/e7231855784bc087155b13889ab59cfdee0d9e92))


### Bug Fixes

* don't exit silently when no cluster is reachable at startup ([#58](https://github.com/fmidev/kubetin/issues/58)) ([2c21274](https://github.com/fmidev/kubetin/commit/2c21274640e6c8646280473469dab1b76d13d24c))
* let the stacked dashboard's log pane fill a tall window ([#54](https://github.com/fmidev/kubetin/issues/54)) ([8a4eeda](https://github.com/fmidev/kubetin/commit/8a4eeda0d1de7425ecf09e6d66b97f4458a974ea))
* scroll the focused pane in the stacked dashboard ([#56](https://github.com/fmidev/kubetin/issues/56)) ([deeec37](https://github.com/fmidev/kubetin/commit/deeec3741d28f8822aed443ce5769585edf3dd11))
* stop pod log colours from corrupting the screen ([#59](https://github.com/fmidev/kubetin/issues/59)) ([4ee0f65](https://github.com/fmidev/kubetin/commit/4ee0f653bb816491ee8be5cd74f2abe68ff41ed2))


### Performance Improvements

* format only the event rows the pane displays ([#57](https://github.com/fmidev/kubetin/issues/57)) ([ac9f273](https://github.com/fmidev/kubetin/commit/ac9f273252d4b4860de97c0fc753561237a75182))

## [1.7.0](https://github.com/fmidev/kubetin/compare/v1.6.0...v1.7.0) (2026-08-26)


### Features

* hide the cluster rail when it earns nothing ([#47](https://github.com/fmidev/kubetin/issues/47)) ([555bb72](https://github.com/fmidev/kubetin/commit/555bb72aedacf051b79e0abdee54101442c8b32a))
* services and ingresses views ([#49](https://github.com/fmidev/kubetin/issues/49)) ([dd296e8](https://github.com/fmidev/kubetin/commit/dd296e8f5756b53a56673d7213628215a9f1028f))

## [1.6.0](https://github.com/fmidev/kubetin/compare/v1.5.0...v1.6.0) (2026-08-26)


### Features

* project richer pod/deployment fields from informers ([#39](https://github.com/fmidev/kubetin/issues/39)) ([16ca9dc](https://github.com/fmidev/kubetin/commit/16ca9dca39513b6743d5a88455563a5c5666d8cc))
* status dashboard for deployments ([#41](https://github.com/fmidev/kubetin/issues/41)) ([fa6c147](https://github.com/fmidev/kubetin/commit/fa6c147920348a9c3eb69d63508265344ff7c016))
* status dashboard for pods ([#40](https://github.com/fmidev/kubetin/issues/40)) ([5d3711b](https://github.com/fmidev/kubetin/commit/5d3711bc579e76fec334e72b3db74911786c7ee7))


### Bug Fixes

* stop informer updates blanking pod CPU/MEM and network rates ([#42](https://github.com/fmidev/kubetin/issues/42)) ([59215f1](https://github.com/fmidev/kubetin/commit/59215f19b1e80cfd8c648fb4ceb82cd72100a878))

## [1.5.0](https://github.com/fmidev/kubetin/compare/v1.4.0...v1.5.0) (2026-06-15)


### Features

* responsive table columns — degrade on narrow panes, grow on wide ([#37](https://github.com/fmidev/kubetin/issues/37)) ([f4f8353](https://github.com/fmidev/kubetin/commit/f4f8353c83f30d83ea0b4369a188b77c672023d5))

## [1.4.0](https://github.com/fmidev/kubetin/compare/v1.3.0...v1.4.0) (2026-05-12)


### Features

* OpenShift Project view (closes [#29](https://github.com/fmidev/kubetin/issues/29)) ([#35](https://github.com/fmidev/kubetin/issues/35)) ([be4617d](https://github.com/fmidev/kubetin/commit/be4617d462d49806347b985f0b8ad0da3bdf89ca))

## [1.3.0](https://github.com/fmidev/kubetin/compare/v1.2.0...v1.3.0) (2026-05-12)


### Features

* namespace listing view with action menu ([#28](https://github.com/fmidev/kubetin/issues/28)) ([63bac1c](https://github.com/fmidev/kubetin/commit/63bac1c73a0db9054f3ea5729b1d111665e45af6))
* node cordon / uncordon / drain from the action menu ([#25](https://github.com/fmidev/kubetin/issues/25)) ([a9721a4](https://github.com/fmidev/kubetin/commit/a9721a414be27d5fec5b187413c8a689355927d8))
* per-namespace resource counts (PODS / DEP / WRN) ([#31](https://github.com/fmidev/kubetin/issues/31)) ([783c156](https://github.com/fmidev/kubetin/commit/783c1565e1ff772e3ef74a00ca5591162297d5e8))
* RBAC visibility — overlay + inline action-menu state ([#33](https://github.com/fmidev/kubetin/issues/33)) ([8e59c6b](https://github.com/fmidev/kubetin/commit/8e59c6b424ef0d2102e47e207e51e9afe7ad823b))
* redesign action menu — colourful layout + floating overlay ([#34](https://github.com/fmidev/kubetin/issues/34)) ([aa656eb](https://github.com/fmidev/kubetin/commit/aa656eb5b87e7d0a3f88f4d677256f3808c5e342))
* sortable columns in the namespace view ([#32](https://github.com/fmidev/kubetin/issues/32)) ([3e40ec2](https://github.com/fmidev/kubetin/commit/3e40ec2f26c59d4fdf9477e30ca4454e911f33f6))

## [1.2.0](https://github.com/fmidev/kubetin/compare/v1.1.0...v1.2.0) (2026-05-11)


### Features

* exec into a container from the action menu ([#24](https://github.com/fmidev/kubetin/issues/24)) ([ca7f13a](https://github.com/fmidev/kubetin/commit/ca7f13a750a12687b53e1425284b7e52bab64d69))
* scoped events view from the action menu ([#9](https://github.com/fmidev/kubetin/issues/9)) ([fca8055](https://github.com/fmidev/kubetin/commit/fca8055763a24732caf48a18a21e55c91ef860d4))


### Bug Fixes

* sort events by LastSeen, stable, with Reason tie-breaker ([#10](https://github.com/fmidev/kubetin/issues/10)) ([7641f60](https://github.com/fmidev/kubetin/commit/7641f60088ad9fc86cf4ce7786892bb6404d7252))

## [1.1.0](https://github.com/fmidev/kubetin/compare/v1.0.0...v1.1.0) (2026-05-10)


### Features

* thin separators under top bar and between sidebar clusters ([#5](https://github.com/fmidev/kubetin/issues/5)) ([7f6c8a5](https://github.com/fmidev/kubetin/commit/7f6c8a572a928a81789059879620fc095d752fdc))


### Bug Fixes

* keep selection highlight on across the whole row ([#4](https://github.com/fmidev/kubetin/issues/4)) ([af24d6d](https://github.com/fmidev/kubetin/commit/af24d6d781009760fa86ab34094dcee9c06c3b1b))
* prompt to re-trust changed kubeconfigs instead of warning ([#2](https://github.com/fmidev/kubetin/issues/2)) ([b950234](https://github.com/fmidev/kubetin/commit/b9502348edf4150fd3a4526e764f3478c651966a))
* use unix.Dup2 so linux/arm64 release build links ([#6](https://github.com/fmidev/kubetin/issues/6)) ([9892d7d](https://github.com/fmidev/kubetin/commit/9892d7d98ff52c2ed05811b8924360a145f80937))
