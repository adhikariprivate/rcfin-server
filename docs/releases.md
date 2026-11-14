# Releases

Pushing a version tag matching `v*` to GitHub triggers `.github/workflows/release.yml`. The workflow uses GoReleaser to compile RCFin with CGO disabled and publishes the platform archives plus `checksums.txt` on the matching GitHub Release.

## Supported targets

Release builds include the Go-supported targets below. ARM 32-bit Linux and BSD builds are generated for ARMv6 and ARMv7 where supported by Go.

| Operating system | Architectures |
| --- | --- |
| Linux | 386, amd64, armv6, armv7, arm64 |
| Windows | 386, amd64, arm64 |
| FreeBSD | 386, amd64, armv6, armv7, arm64 |
| OpenBSD | amd64, arm64 |
| NetBSD | amd64 |

Each archive contains the `rcfin` executable, license, README, example configuration, contribution guide, and `docs/` documentation. Windows archives are ZIP files; other archives are gzip-compressed tar files. `checksums.txt` contains SHA-256 checksums.

## Verify a release locally

Install Go from `go.mod` and GoReleaser v2, then run:

```sh
goreleaser check
goreleaser release --snapshot --clean
```

The snapshot command builds the full configured matrix and creates local archives without publishing a GitHub Release. To publish, push a semantic version tag such as `v1.0.0`; the workflow creates the release and uploads all artifacts automatically.
