# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](http://keepachangelog.com/)
and this project adheres to [Semantic Versioning](http://semver.org/).

## Unreleased

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
