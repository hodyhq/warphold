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

Checksum signing (`tools/warphold-sign.sh`, invoked as `signs.cmd`) needs two
repository secrets, sourced from 1Password — never commit them:

- `WARPHOLD_SIGNING_KEY` — the private key, ASCII-armored (`gpg --export-secret-keys --armor`)
- `WARPHOLD_SIGNING_PASSPHRASE` — its passphrase

The workflow imports `WARPHOLD_SIGNING_KEY` into a throwaway `GNUPGHOME`
(created fresh, `chmod 700`, removed at the end of the job — never the
runner's default keyring) and derives the key id from the imported key's own
fingerprint, so no third "key id" secret is needed. `WARPHOLD_SIGNING_KEY_ID`
and `GNUPGHOME`/`GPG_TTY` are exported to the goreleaser step only; the
passphrase is only ever read by gpg over `--passphrase-fd`, never as an
argument or echoed.

If either secret is missing, the workflow detects it, prints a `::warning::`,
and runs goreleaser with `--skip=sign` — the release still publishes, with an
**unsigned** `checksums.txt`. Set both secrets before a release that needs
signed checksums. `tools/sign.sh` is upstream Kopia's script (hardcodes the
"Kopia Builder" gpg key name) and is left untouched; `tools/warphold-sign.sh`
is the WarpHold-specific replacement wired into `.goreleaser.yml`.

### Verifying a signed release

Signed `checksums.txt` / RPM signatures are produced by the **WarpHold
Release Signing** key, ed25519, fingerprint
`A6F90B08A0E92752852813E7323C001969AA4FB3`, expires 2028-09-02. The public
key is checked in at
[`docs/warphold-release-signing.asc`](warphold-release-signing.asc); import
it and verify with:

```bash
gpg --import docs/warphold-release-signing.asc
gpg --verify checksums.txt.sig checksums.txt
```

The private key + passphrase live in 1Password (vault `hody`, item
"WarpHold Release Signing Key") and as the `WARPHOLD_SIGNING_KEY` /
`WARPHOLD_SIGNING_PASSPHRASE` repo secrets on `hodyhq/warphold` — never
committed.

## Install paths

Tarball installers (`scripts/install/fleet.sh`, `scripts/install/app.sh`)
install to `/usr/local/bin`; the `.deb`/`.rpm` packages install to
`/usr/bin` (`bindir` in `.goreleaser.yml`'s `nfpms`, the distro norm for
packaged binaries). Both are on `PATH` — do not install both on the same
machine.

## goreleaser version

`.goreleaser.yml` is pinned to the **goreleaser v1 config schema** (no
`version:` key, `overrides`/name-template style unchanged since v1.21) and
CI/local dry runs pin the `goreleaser` binary itself to `1.26.2`, the last v1
release. Don't bump to a v2 `goreleaser` binary or migrate the config to the
v2 schema without repinning both together — v2 removed fields this config
still would have used (`replacements`) and the two schemas aren't
mixable.

## Local dry run

```bash
mise use -g goreleaser@1.26.2
GITHUB_REPOSITORY=hodyhq/warphold KOPIA_VERSION_NO_PREFIX=0.0.0-snapshot \
  goreleaser release --snapshot --clean --skip=sign,publish
```
