# Contributing to RCFin

Thanks for considering a contribution. Changes should preserve the server's Jellyfin client behavior and keep unsupported functionality clearly documented.

## Local setup

1. Install [mise](https://mise.jdx.dev/).
2. Run `mise install` to install the Go version pinned in `mise.toml`.
3. Copy `config.example.yaml` to `rcfin.yaml` and configure a local SQLite path and filesystem library roots. Use `config.example.full.yaml` as the exhaustive setting reference.
4. Run `mise run build` to compile the server or `mise run test` to run the Go test suite.

## Repository structure

- `cmd/rcfin-server` contains the executable entry point and server startup wiring.
- `internal/` contains RCFin's application packages, including collection scanning, database implementations, Jellyfin handlers, image caching, and Rclone storage.
- `docs/` contains operator and architecture documentation.

The Go `internal` boundary keeps implementation packages private to this module. There is no `pkg/` directory because RCFin does not currently publish a reusable Go library API; add one only when a package is intentionally supported for use by other modules.

The `rcfin.yaml` file is ignored by Git. Do not commit credentials, private library paths, or generated database/cache files.

## Before opening a change

- Keep changes focused and explain the user-visible behavior they affect.
- Add or update tests for behavior changes.
- Run `mise run test` and `mise run build` when the local environment supports them.
- Update README or configuration examples when behavior or configuration changes.
- Keep Jellyfin compatibility claims limited to endpoints and client behavior that have been verified.

## Pull requests

Include a concise summary, the reason for the change, and the commands used to verify it. For protocol changes, include the relevant Jellyfin endpoint and a client or request example when possible.

By submitting a contribution, you agree that it may be distributed under the project's MIT License.
