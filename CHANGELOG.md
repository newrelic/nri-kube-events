# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](http://keepachangelog.com/)
and this project adheres to [Semantic Versioning](http://semver.org/).

## Unreleased

### Added
- Comprehensive global value inheritance test coverage (42 new tests) validating 21/27 applicable global values @dpacheconr [#516](https://github.com/newrelic/nri-kube-events/pull/516)
- Test coverage for all agent configuration values (proxy, customAttributes, nrStaging, verboseLog) @dpacheconr [#516](https://github.com/newrelic/nri-kube-events/pull/516)
- Resource sizing guidance for small, medium, and large Kubernetes clusters @dpacheconr [#516](https://github.com/newrelic/nri-kube-events/pull/516)
- Documentation for global value override patterns and precedence rules @dpacheconr [#516](https://github.com/newrelic/nri-kube-events/pull/516)

### Changed
- Regenerated README.md with helm-docs for improved documentation consistency @dpacheconr [#516](https://github.com/newrelic/nri-kube-events/pull/516)

## v2.16.2 - 2025-11-24

### ⛓️ Dependencies
- Updated go to v1.25.4
- Updated kubernetes packages to v0.34.2

## v2.16.1 - 2025-10-20

### ⛓️ Dependencies
- Updated go to v1.25.3

## v2.16.0 - 2025-10-13

### dependency
- Upgrade k8s.io/api, apimachinery, client-go, kubectl to v0.34.1 @TmNguyen12 [#505](https://github.com/newrelic/nri-kube-events/pull/505)

### 🚀 Enhancements
- Add support for k8s v1.34.0, remove support for v1.29.5 @TmNguyen12 [#505](https://github.com/newrelic/nri-kube-events/pull/505)

### ⛓️ Dependencies
- Updated alpine to v3.22.2

## v2.15.2 - 2025-09-15

### ⛓️ Dependencies
- Updated go to v1.25.1
- Updated actions/setup-go to v6
- Updated github.com/prometheus/client_golang to v1.23.2 - [Changelog 🔗](https://github.com/prometheus/client_golang/releases/tag/v1.23.2)

## v2.15.1 - 2025-09-08

### ⛓️ Dependencies
- Updated actions/download-artifact to v5
- Updated golang version
- Updated github.com/prometheus/client_golang to v1.23.1 - [Changelog 🔗](https://github.com/prometheus/client_golang/releases/tag/v1.23.1)

## v2.15.0 - 2025-08-11

### 🚀 Enhancements
- Add v1.33 support and drop support for v1.28 @TmNguyen12 [#492](https://github.com/newrelic/nri-kube-events/pull/492)

## v2.14.0 - 2025-08-04

### 🚀 Enhancements
- Improve stability of GHAs @dbudziwojskiNR [#488](https://github.com/newrelic/nri-kube-events/pull/488)

### ⛓️ Dependencies
- Updated github.com/prometheus/client_golang to v1.23.0 - [Changelog 🔗](https://github.com/prometheus/client_golang/releases/tag/v1.23.0)
- Updated kubernetes packages to v0.33.3

## v2.13.4 - 2025-07-21

### ⛓️ Dependencies
- Updated alpine to v3.22.1

## v2.13.3 - 2025-07-14

### ⛓️ Dependencies
- Updated kubernetes packages to v0.33.2
- Updated go to v1.24.5

## v2.13.2 - 2025-06-23

### ⛓️ Dependencies
- Updated go to v1.24.4

## v2.13.1 - 2025-06-02

### ⛓️ Dependencies
- Updated alpine to v3.22.0

## v2.13.0 - 2025-05-19

### ⛓️ Dependencies
- Updated github.com/prometheus/client_model to v0.6.2 - [Changelog 🔗](https://github.com/prometheus/client_model/releases/tag/v0.6.2)
- Updated golang version
- Updated go to v1.24.3
- Upgraded golang.org/x/net from 0.33.0 to 0.36.0

## v2.12.1 - 2025-04-21

### ⛓️ Dependencies
- Updated github.com/prometheus/client_golang to v1.22.0 - [Changelog 🔗](https://github.com/prometheus/client_golang/releases/tag/v1.22.0)

## v2.12.0 - 2025-03-24

### 🚀 Enhancements
- Add v1.32 support and drop support for v1.27 @kpattaswamy [#463](https://github.com/newrelic/nri-kube-events/pull/463)

### ⛓️ Dependencies
- Updated kubernetes packages to v0.32.3

## v2.11.8 - 2025-03-10

### ⛓️ Dependencies
- Updated github.com/prometheus/client_golang to v1.21.1 - [Changelog 🔗](https://github.com/prometheus/client_golang/releases/tag/v1.21.1)
- Updated kubernetes packages to v0.32.2

## v2.11.7 - 2025-02-24

### ⛓️ Dependencies
- Updated github.com/prometheus/client_golang to v1.21.0 - [Changelog 🔗](https://github.com/prometheus/client_golang/releases/tag/v1.21.0)
- Updated alpine to v3.21.3

## v2.11.6 - 2025-01-20

### ⛓️ Dependencies
- Updated kubernetes packages to v0.32.1
- Updated go to v1.23.5

## v2.11.5 - 2025-01-13

### ⛓️ Dependencies
- Updated alpine to v3.21.2

## v2.11.4 - 2024-12-23

### ⛓️ Dependencies
- Updated kubernetes packages to v0.32.0
- Updated go to v1.23.4

## v2.11.3 - 2024-12-09

### ⛓️ Dependencies
- Updated alpine to v3.21.0

## v2.11.2 - 2024-11-18

### 🐞 Bug fixes
- Update and clean linters @dbudziwojskiNR [#441](https://github.com/newrelic/nri-kube-events/pull/441)

### ⛓️ Dependencies
- Updated go to v1.23.3

## v2.11.1 - 2024-11-04

### ⛓️ Dependencies
- Updated kubernetes packages to v0.31.2

## v2.11.0 - 2024-10-28

### 🚀 Enhancements
- Add 1.31 support and drop 1.26 @zeitlerc [#434](https://github.com/newrelic/nri-kube-events/pull/434)

## v2.10.9 - 2024-10-21

### ⛓️ Dependencies
- Updated github.com/prometheus/client_golang to v1.20.5 - [Changelog 🔗](https://github.com/prometheus/client_golang/releases/tag/v1.20.5)

## v2.10.8 - 2024-10-07

### ⛓️ Dependencies
- Updated kubernetes packages to v0.31.1
- Updated go to v1.23.2

## v2.10.7 - 2024-09-23

### ⛓️ Dependencies
- Updated github.com/prometheus/client_golang to v1.20.4 - [Changelog 🔗](https://github.com/prometheus/client_golang/releases/tag/v1.20.4)

## v2.10.6 - 2024-09-09

### ⛓️ Dependencies
- Updated github.com/prometheus/client_golang to v1.20.3 - [Changelog 🔗](https://github.com/prometheus/client_golang/releases/tag/v1.20.3)
- Updated alpine to v3.20.3

## v2.10.5 - 2024-09-02

### ⛓️ Dependencies
- Updated golang version
- Updated kubernetes packages to v0.31.0
- Updated github.com/prometheus/client_golang to v1.20.2 - [Changelog 🔗](https://github.com/prometheus/client_golang/releases/tag/v1.20.2)

## v2.10.4 - 2024-08-19

### ⛓️ Dependencies
- Updated github.com/prometheus/client_golang to v1.20.0 - [Changelog 🔗](https://github.com/prometheus/client_golang/releases/tag/v1.20.0)

## v2.10.3 - 2024-08-12

### ⛓️ Dependencies
- Updated go to v1.22.5

## v2.10.2 - 2024-07-29

### ⛓️ Dependencies
- Updated alpine to v3.20.2
- Updated kubernetes packages to v0.30.3

## v2.10.1 - 2024-07-08

### ⛓️ Dependencies
- Updated kubernetes packages to v0.30.2

## v2.10.0 - 2024-06-24

### 🚀 Enhancements
- Add 1.29 and 1.30 support and drop 1.25 and 1.24 @dbudziwojskiNR [#409](https://github.com/newrelic/nri-kube-events/pull/409)

### ⛓️ Dependencies
- Updated alpine to v3.20.1

## v2.9.10 - 2024-06-17

### ⛓️ Dependencies
- Updated go to v1.22.4

## v2.9.9 - 2024-06-10

### ⛓️ Dependencies
- Updated go to v1.22.3
- Updated kubernetes packages to v0.30.1

## v2.9.8 - 2024-05-27

### ⛓️ Dependencies
- Updated alpine to v3.20.0

## v2.9.7 - 2024-05-13

### ⛓️ Dependencies
- Updated github.com/prometheus/client_golang to v1.19.1 - [Changelog 🔗](https://github.com/prometheus/client_golang/releases/tag/v1.19.1)

## v2.9.6 - 2024-04-29

### ⛓️ Dependencies
- Updated kubernetes packages to v0.30.0

## v2.9.5 - 2024-04-22

### ⛓️ Dependencies
- Updated go to v1.22.2

## v2.9.4 - 2024-04-15

### ⛓️ Dependencies
- Updated github.com/prometheus/client_model to v0.6.1 - [Changelog 🔗](https://github.com/prometheus/client_model/releases/tag/v0.6.1)

## v2.9.3 - 2024-03-25

### ⛓️ Dependencies
- Updated kubernetes packages to v0.29.3
- Updated go to v1.22.1

## v2.9.2 - 2024-03-11

### ⛓️ Dependencies
- Updated golang version

## v2.9.1 - 2024-03-04

### ⛓️ Dependencies
- Updated kubernetes packages to v0.29.2
- Updated github.com/prometheus/client_model to v0.6.0 - [Changelog 🔗](https://github.com/prometheus/client_model/releases/tag/v0.6.0)
- Updated github.com/prometheus/client_golang to v1.19.0 - [Changelog 🔗](https://github.com/prometheus/client_golang/releases/tag/v1.19.0)

## v2.9.0 - 2024-02-26

### 🚀 Enhancements
- Add linux node selector @dbudziwojskiNR [#378](https://github.com/newrelic/nri-kube-events/pull/378)

## v2.8.2 - 2024-02-19

### ⛓️ Dependencies
- Updated github.com/newrelic/infra-integrations-sdk to v3.8.2+incompatible

## v2.8.1 - 2024-02-12

### ⛓️ Dependencies
- Updated github.com/newrelic/infra-integrations-sdk to v3.8.0+incompatible

## v2.8.0 - 2024-02-05

### 🚀 Enhancements
- Add Codecov @dbudziwojskiNR [#364](https://github.com/newrelic/nri-kube-events/pull/364)

## v2.7.4 - 2024-01-29

### ⛓️ Dependencies
- Updated kubernetes packages to v0.29.1
- Updated alpine to v3.19.1

## v2.7.3 - 2024-01-22

### ⛓️ Dependencies
- Updated go to v1.21.6

## v2.7.2 - 2024-01-09

### ⛓️ Dependencies
- Updated kubernetes packages to v0.29.0

## v2.7.1 - 2024-01-01

### ⛓️ Dependencies
- Updated github.com/prometheus/client_golang to v1.18.0 - [Changelog 🔗](https://github.com/prometheus/client_golang/releases/tag/v1.18.0)

## v2.7.0 - 2023-12-09

### 🚀 Enhancements
- Trigger release creation by @juanjjaramillo [#351](https://github.com/newrelic/nri-kube-events/pull/351)

### ⛓️ Dependencies
- Updated alpine to v3.19.0
- Updated go to v1.21.5

## v2.6.0 - 2023-12-06

### 🚀 Enhancements
- Update reusable workflow dependency by @juanjjaramillo [#346](https://github.com/newrelic/nri-kube-events/pull/346)

### ⛓️ Dependencies
- Updated kubernetes packages to v0.28.4

## v2.5.0 - 2023-11-20

### 🚀 Enhancements
- Create E2E workflow by @juanjjaramillo in [#343](https://github.com/newrelic/nri-kube-events/pull/343)
- Create E2E tests by @juanjjaramillo in [#342](https://github.com/newrelic/nri-kube-events/pull/342)
- Create E2E resources Helm chart by @juanjjaramillo in [#341](https://github.com/newrelic/nri-kube-events/pull/341)

## v2.4.0 - 2023-11-13

### 🚀 Enhancements
- Replace k8s v1.28.0-rc.1 with k8s 1.28.3 support by @svetlanabrennan in [#338](https://github.com/newrelic/nri-kube-events/pull/338)

## v2.3.0 - 2023-10-30

### 🚀 Enhancements
- Remove 1.23 support by @svetlanabrennan in [#330](https://github.com/newrelic/nri-kube-events/pull/330)
- Add k8s 1.28.0-rc.1 support by @svetlanabrennan in [#330](https://github.com/newrelic/nri-kube-events/pull/331)

## v2.2.13 - 2023-10-23

### ⛓️ Dependencies
- Updated kubernetes packages to v0.28.3

## v2.2.12 - 2023-10-16

### 🐞 Bug fixes
- Address CVE-2023-44487 and CVE-2023-39325 by juanjjaramillo in [#326](https://github.com/newrelic/nri-kube-events/pull/326)

## v2.2.11 - 2023-10-09

### ⛓️ Dependencies
- Updated github.com/prometheus/client_model to v0.5.0 - [Changelog 🔗](https://github.com/prometheus/client_model/releases/tag/v0.5.0)

## v2.2.10 - 2023-10-02

### ⛓️ Dependencies
- Updated github.com/prometheus/client_model digest to baaa038

## v2.2.9 - 2023-09-30

### ⛓️ Dependencies
- Updated github.com/prometheus/client_model digest to baaa038
- Updated alpine to v3.18.4
- Updated github.com/prometheus/client_model digest

## v2.2.8 - 2023-09-29

### ⛓️ Dependencies
- Updated github.com/prometheus/client_golang to v1.17.0 - [Changelog 🔗](https://github.com/prometheus/client_golang/releases/tag/v1.17.0)

## v2.2.7 - 2023-09-21

### 🐞 Bug fixes
- Update `GITHUB_TOKEN` permissions to allow for chart releasing by @juanjjaramillo in [#305](https://github.com/newrelic/nri-kube-events/pull/305)

## v2.2.6 - 2023-09-20

### 🐞 Bug fixes
- Update CHANGELOG.md

## v2.2.4 - 2023-09-20

### 🐞 Bug fixes
- Update GITHUB_TOKEN permissions to allow for job to open a PR by @juanjjaramillo in [#303](https://github.com/newrelic/nri-kube-events/pull/303)

## v2.2.3 - 2023-09-20

### 🐞 Bug fixes
- Make changelog.yml workflow run when labels change by @juanjjaramillo in [#301](https://github.com/newrelic/nri-kube-events/pull/301)
- Add debugging information to the workflow by @juanjjaramillo in [#300](https://github.com/newrelic/nri-kube-events/pull/300)

## v2.2.2 - 2023-09-15

### 🐞 Bug fixes
- Fix job step in release.yml workflow by @juanjjaramillo in [#296](https://github.com/newrelic/nri-kube-events/pull/296)

## v2.2.1 - 2023-09-15

### 🐞 Bug fixes
- Fix typo in release.yml workflow by @juanjjaramillo in [#293](https://github.com/newrelic/nri-kube-events/pull/293)

## v2.2.0 - 2023-09-15

### 🚀 Enhancements
- Update K8s Versions in E2E Tests by @xqi-nr in [#275](https://github.com/newrelic/nri-kube-events/pull/275)

### ⛓️ Dependencies
- Updated kubernetes packages to v0.28.2
- Updated golang version

## 2.1.2

## What's Changed
* Update Changelog by @xqi-nr in https://github.com/newrelic/nri-kube-events/pull/257
* Bump Chart Versions by @xqi-nr in https://github.com/newrelic/nri-kube-events/pull/258
* chore(deps): update newrelic/k8s-events-forwarder docker tag to v1.43.1 by @renovate in https://github.com/newrelic/nri-kube-events/pull/259
* Upgrade Apimachinery Lib by @xqi-nr in https://github.com/newrelic/nri-kube-events/pull/260


**Full Changelog**: https://github.com/newrelic/nri-kube-events/compare/v2.1.1...v2.1.2

## 2.1.1

### What's Changed
* Bump app and chart version by @vihangm in https://github.com/newrelic/nri-kube-events/pull/250
* chore(deps): update newrelic/k8s-events-forwarder docker tag to v1.43.0 by @renovate in https://github.com/newrelic/nri-kube-events/pull/251
* chore(deps): bump github.com/sirupsen/logrus from 1.9.2 to 1.9.3 by @dependabot in https://github.com/newrelic/nri-kube-events/pull/252
* chore(deps): bump github.com/stretchr/testify from 1.8.3 to 1.8.4 by @dependabot in https://github.com/newrelic/nri-kube-events/pull/253
* chore(deps): bump alpine from 3.18.0 to 3.18.2 by @dependabot in https://github.com/newrelic/nri-kube-events/pull/254
* chore(deps): bump github.com/prometheus/client_golang from 1.15.1 to 1.16.0 by @dependabot in https://github.com/newrelic/nri-kube-events/pull/255


**Full Changelog**: https://github.com/newrelic/nri-kube-events/compare/v2.1.0...v2.1.1



## 1.9.3

### Added

- Add Resource Settings for Kube-events' Forwarder (#198)

### Changed

- Update docker build
- Update various dependencies

Full Changelog: https://github.com/newrelic/nri-kube-events/compare/v1.9.2...v1.9.3

## 1.9.2

### Changed

- Bump alpine version to address vulnerability
- Bump various go deps to address vulnerabilities

## 1.9.1

### Changed

- Bump alpine version to address vulnerability

## 1.9.0

### Changed

- Updated go version and dependencies

## 1.8.0

### Changed

- Updated dependencies
- IntegrationVersion is now automatically populated and included in the sample

## 1.7.0

### Changed

- Updated dependencies

## 1.6.0

### Changed

- Adds Kubernetes 1.22 dependencies updates

## 1.5.1

### Changed

- Kubernetes client dependencies have been upgraded to ensure compatibility with the latest versions

## 1.5.0

### Changed

- Docker images now support multiple architectures (linux/amd64, linux/arm64)

## 1.5.0

### Changed

- Docker images now support multiple architectures (linux/amd64, linux/arm64)

## 1.4.0

### Changed

- Updated all dependencies to their latest versions

### Fixed

- `k8s.io/client-go` will no longer attempt to write logs to `/tmp`

## 1.3.2

### Changed

- Moving pipelines to Github Actions.

## 1.3.0

### Changed

- Update newrelic/k8s-events-forwarder to version `1.12.0`.

## 1.2.0

### Changed

- Update newrelic/k8s-events-forwarder to version `1.11.45`.

## 1.1.0

### Changed

- Update base image to alpine `3.11`.
- Update newrelic/k8s-events-forwarder to version `1.11.24`.
- Move manifest from `apps/v1beta2` to `apps/v1`
- Sync labels in helm chart and manifest. Use `nri-kube-events` in all cases.
  **IMPORTANT:** If you previously installed `nr-kube-events` using the manifest you should uninstall it first with the OLD manifest before applying the new one. Users of our wizard can upgrade normally.

## 1.0.0

### Added

- Add custom attributes support. Custom attributes are added via environment
  variables of the form `NRI_KUBE_EVENTS_<key>=<val>`.
