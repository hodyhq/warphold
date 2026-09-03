# Releasing

Pushing a `v*` tag runs `.github/workflows/release.yml`:

1. Compiles darwin/arm64 and windows/amd64 as portability checks (`go build`
   only — never published; WarpHold ships Linux only, see D8).
2. Runs `goreleaser release` for `linux/amd64` and `linux/arm64`: tarballs,
   `.deb`/`.rpm` (via `.goreleaser.yml`'s `nfpms`), `checksums.txt`, and the
   two install scripts (`scripts/install/fleet.sh`, `scripts/install/app.sh`)
   attached as release assets so `releases/latest/download/<script>.sh`
   always matches the latest binary.

## Signing key (human checkpoint)

Checksum signing (`tools/sign.sh`) needs two repository secrets, sourced from
1Password — never commit them:

- `WARPHOLD_SIGNING_KEY`
- `WARPHOLD_SIGNING_PASSPHRASE`

If either is missing, the workflow detects it, prints a `::warning::`, and
runs goreleaser with `--skip=sign` — the release still publishes, with an
**unsigned** `checksums.txt`. Set both secrets before a release that needs
signed checksums.

## Local dry run

```bash
mise use -g goreleaser@1.26.2
GITHUB_REPOSITORY=hodyhq/warphold KOPIA_VERSION_NO_PREFIX=0.0.0-snapshot \
  goreleaser release --snapshot --clean --skip=sign,publish
```
