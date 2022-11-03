# Change Log

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](http://keepachangelog.com/)
and this project adheres to [Semantic Versioning](http://semver.org/).

## 1.8.1
### Changed

- Bump alpine version to address vulnerability

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
