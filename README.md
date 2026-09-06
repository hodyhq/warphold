<p align="center"><img src="icons/warphold.svg" width="96" alt="WarpHold"></p>

# WarpHold

**Backups that hold.**

WarpHold backs up the machines you care about — laptops, desktops, the box in the
basement — with client-side encryption, deduplication and browsable snapshot
history. Run it on one computer, or run **Fleet** on one server and enroll every
other machine in the house or the office, so you can see at a glance whether all
of them are still backing up.

> **Early access.** Linux (`amd64`, `arm64`) is the only published platform today;
> Windows and macOS agents are planned. Documentation lives at
> [warphold.com](https://warphold.com) and [docs.warphold.com](https://docs.warphold.com).

---

## Single machine

```sh
curl -fsSL https://get.warphold.com/app.sh | sh
```

Or install a `.deb` / `.rpm` from [Releases](https://github.com/hodyhq/warphold/releases).
Either way you get the binary, a user-scope backup engine, a tray icon, and the
app open in your browser. The single-machine app never runs a Fleet server.

Every release's `checksums.txt` is signed by the **WarpHold Release Signing**
key (ed25519, fingerprint `A6F90B08 A0E92752 852813E7 323C0019 69AA4FB3`,
expires 2028-09-02):

```sh
gpg --import docs/warphold-release-signing.asc
gpg --verify checksums.txt.sig checksums.txt
```

See [docs/RELEASING.md](docs/RELEASING.md#verifying-a-signed-release) for the
full verification steps.

![The single-machine app: every source this computer backs up](https://raw.githubusercontent.com/hodyhq/warphold-ui/main/docs/screenshots/solo-snapshots@1440.png)

![Restoring a snapshot to a directory, an archive, or a mount](https://raw.githubusercontent.com/hodyhq/warphold-ui/main/docs/screenshots/solo-restore@1440.png)

- **Snapshots on a schedule** of any directory you point at, with retention rules
  and exclude patterns per source.
- **Browse and restore** inside any snapshot — a single file, a whole tree, a
  `.tar`/`.zip`, or a live mount.
- **Your storage, your choice:** local disk or NAS, Amazon S3 and S3-compatible
  services, Backblaze B2, Azure Blob Storage, Google Cloud Storage, SFTP, WebDAV,
  and some Rclone remotes.
- **Encrypted before it leaves the machine,** compressed and deduplicated — the
  engine is Kopia's, unchanged.
- **Tray** showing the last and next run, with the task list a click away.
- **Works on a phone screen** as well as a desktop one; every screen is responsive.

## Fleet

```sh
curl -fsSL https://get.warphold.com/fleet.sh | sh
```

The installer creates the service user and directories, writes the systemd unit,
starts the server and prints the URL and setup token. Open that URL and the
browser walks you through the rest: sealing passphrase, first admin, the public
URL it will hand to devices, where the backups live — and then it hands you the
one-line command that enrolls the first device.

![The fleet dashboard: health counts, the last 24 hours and a 30-day strip per device](https://raw.githubusercontent.com/hodyhq/warphold-ui/main/docs/screenshots/fleet-overview@1440.png)

![One device: its sources, recent runs and the commands an admin can send it](https://raw.githubusercontent.com/hodyhq/warphold-ui/main/docs/screenshots/fleet-device@1440.png)

- **Dashboard** — health counts, the last 24 hours, and a 30-day strip per device,
  so a machine that quietly stopped backing up is visible in one glance.
- **Devices** — every enrolled machine with its group, health and last good
  backup; open one for its sources, its recent runs, the error it reported, and
  the commands an admin can send it.
- **Groups** tie a policy template to a storage target. Adding a device to a
  group issues a one-shot token and prints the command to run on it.
- **Policy templates** — what to back up, how often, how long to keep it — pushed
  to every device in the group.
- **Storage targets** — *hosted*, where devices back up to the Fleet server
  itself (Fleet disk, with an optional mirror to an Object-Lock bucket), or
  *cloud-direct*, where Fleet's own bucket credentials write straight to the
  customer's bucket and devices never hold cloud credentials. Backblaze B2
  works as a mirror target; cloud-direct needs a provider with real
  conditional writes (`If-None-Match`), which B2's S3 endpoint doesn't
  implement — use AWS S3, Cloudflare R2, MinIO, or another S3-compatible
  service that does.
- **Complete per-device isolation** — one repository per device, one key per
  device (see below).
- **Jobs**, run on a schedule per target: `verify` (weekly, `snapshot verify`
  against the repository), `test-restore` (monthly, restores a random file and
  checks its hash), `maintenance` (daily, so devices never run their own),
  `mirror` (hourly, for mirrored targets), `stats` (daily, feeds the Stored
  tiles), `digest` (weekly fleet-status email over SMTP), and `reap` (removes a
  revoked device's repository after its retention window). Run any of them on
  demand: `warphold fleet jobs run --kind verify --agent <id>`.
- **A recovery kit per device:** a print-ready page with the repository location,
  its password, read-only credentials and literal restore commands.
- **Tray and agent page** on each enrolled device, so the person using it can see
  its own schedule and runs without a Fleet login.

## Security model

- **One repository per device, one key per device.** Nothing is shared between
  devices — not a key, not a repository, not a content index. Dedup is per
  device; a family fleet's cross-machine duplication is small next to the blast
  radius of a shared repository.
- **The hosted path is append-only.** Devices talk S3 to a Fleet gateway that
  will not let a device's key overwrite history, and allows `DeleteObject` only
  for the narrow set of blob classes Kopia genuinely needs to complete a
  snapshot. That is stronger than a plain bucket writer key: B2's delete is a
  file *hide*, which needs only write permission.
- **Offsite copies sit under Object Lock,** verified when the target is
  configured, so retention outlives a key that gets compromised.
- **One sealing passphrase** protects the escrow. Every escrowed repository
  password and stored credential is sealed with a key derived from it, and it is
  never stored. Losing it loses the escrow, not the backups.
- **The Fleet admin can decrypt every enrolled device's backups.** Fleet holds
  the admin key for every target it provisions, which is how it runs maintenance
  and generates recovery kits on a device's behalf. For a family or a small
  office that is the point — but it makes the sealing passphrase the one secret
  that must never leak.
- **Standalone restore, always.** A recovery kit plus a stock upstream `kopia`
  binary restores any device completely offline. [CI enforces
  this](.github/workflows/standalone-restore.yml) on every change: it enrolls a
  device, snapshots it, then restores with a pinned upstream `kopia` release
  binary and fails the build if the restore ever needs anything from WarpHold.
  Fleet is a control plane, never a dependency of your data.

## Built on Kopia

WarpHold is a fork of [Kopia](https://github.com/kopia/kopia), licensed under the
Apache License 2.0. The backup engine, the repository format and the client-side
encryption are Kopia's and are used unchanged; WarpHold adds the Fleet control
plane, the device agent, the tray and a rebuilt UI. Upstream changes are merged
regularly — see [docs/superpowers/UPSTREAM.md](docs/superpowers/UPSTREAM.md).

Modified upstream files are marked with `warphold:` comments; new code lives
under `fleet/`, `agent/` and `cli/command_{fleet,agent}_*.go`. See
[NOTICE](NOTICE) for attribution and [LICENSE](LICENSE) for the full license
text. WarpHold is not affiliated with or endorsed by the Kopia project, and does
not use its name or logo as branding.

## Operations notes

- **Bind to `127.0.0.1` or a LAN address behind a TLS reverse proxy.** The Fleet
  server speaks plain HTTP by design and expects Traefik, Caddy or nginx to
  terminate in front of it; without that, enrollment bearer tokens and the setup
  token travel in the clear. `fleet.sh` prints the proxy requirements it needs.
- **Pass server secrets through the environment,** not on the command line —
  `KOPIA_SERVER_PASSWORD` and `KOPIA_SERVER_CONTROL_PASSWORD` behind
  `--server-password` / `--server-control-password`. Arguments are visible to
  every user on the host in `ps` and are recorded in shell history; another
  user's environment is not readable. Without a control password the control API
  is open to anyone who can reach the port.
- **Enrollment tokens deserve the same care.** `sh -c "$(curl -fsSL …/enroll.sh)"`
  with the token read from a hidden prompt into `WARPHOLD_ENROLL_TOKEN` keeps it
  out of `ps` and out of your history; `sh -s -- --token <TOKEN>` does not.
- **Activation is one-shot.** If it fails half-way (key file or database present
  but unusable), copy the whole state directory first (`cp -a <state dir> <state
  dir>.bak`), then remove `seal.key` and `fleet.db` before retrying. This is only
  safe before any device has enrolled — afterwards that key file protects real
  escrowed passwords, which is why WarpHold refuses to overwrite it.
- **The Electron desktop app (`app/`)** is upstream KopiaUI packaging. WarpHold
  neither builds nor ships it; `warphold agent tray` replaces it on Linux.

## Building and contributing

Building from source: [BUILD.md](BUILD.md); release and signing notes ship with
the release workflow under `docs/`. Contributions to the engine follow
[Kopia's contribution guidelines](https://kopia.io/docs/contribution-guidelines/).

**Reporting security issues.** Report issues in WarpHold's own code — Fleet, the
gateway, the agent, the enrollment flow — privately through
[GitHub Security Advisories on `hodyhq/warphold`](https://github.com/hodyhq/warphold/security/advisories/new).
Issues in the upstream Kopia engine belong upstream, where
[Kopia's contribution guidelines](https://kopia.io/docs/contribution-guidelines/)
direct disclosures to `security@kopia.io`.
