# Changelog

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
