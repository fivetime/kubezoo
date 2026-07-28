# Changelog

Notable project changes are documented here.

## Unreleased

### Changed

- Adopted Go 1.26 and Alpine Linux 3.24 build/runtime images.
- Reworked local build, unit test, envtest, lint, and container targets.
- Added GitHub Actions for builds, tests, images, CodeQL, dependency review,
  race detection, linting, and vulnerability reporting.
- Added non-root multi-architecture images with SBOM, provenance, and
  attestations.

### Fixed

- Updated stale discovery and service-account tests to match the runtime
  interfaces.
- Added missing build error handling identified by static analysis.
