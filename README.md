<p align="center"><img src="icons/warphold.svg" width="96" alt="WarpHold"></p>

# WarpHold

**Backups that hold. For every machine you care about.**

WarpHold is a fork of [Kopia](https://github.com/kopia/kopia) — same engine, same repository format, same client-side encryption — with a rebuilt UI and a **Fleet** mode: enroll machines, push them a backup policy, escrow their keys, and see at a glance whether every one of them is still backing up.

- **Single machine:** `warphold server start` gives you the WarpHold app for this computer.
- **Fleet:** activate Fleet on one server, then enroll Linux laptops and servers with a one-line installer. Windows and macOS agents are planned.
- **Standalone restore, always:** a recovery kit plus stock upstream `kopia` can restore any device with WarpHold completely offline. Fleet is a control plane, never a dependency of your data.

> WarpHold is not affiliated with the Kopia project. See [NOTICE](NOTICE). Upstream changes are merged regularly ([docs/superpowers/UPSTREAM.md](docs/superpowers/UPSTREAM.md)).

## Status

Plan 1 (Fleet control plane + Linux agent) is complete and running; the WarpHold UI and tray land in Plan 2. Screenshots will follow the UI.

## WarpHold Fleet quick start
WarpHold adds a "Fleet" control plane and a device-side agent on top of Kopia. Activate a Fleet, start its server, and enroll a device:

```bash
# on the Fleet host
export KOPIA_SERVER_CONTROL_PASSWORD="$(head -c 32 /dev/urandom | base64)"   # keep this secret
warphold --config-file /var/lib/warphold/repository.config fleet activate --email admin@example.com
warphold --config-file /var/lib/warphold/repository.config server start \
  --insecure --without-password --no-ui --no-grpc \
  --address 127.0.0.1:51515 \
  --server-control-password "$KOPIA_SERVER_CONTROL_PASSWORD"

# on the device being enrolled, with a token from the Fleet admin API/UI
curl -fsSL https://<fleet-host>/enroll.sh | sh -s -- --token <TOKEN>
```

`--without-password` leaves Kopia's own control API unauthenticated, so **always pass `--server-control-password`** (without it the control API is open to anyone who can reach the port) and **bind to `127.0.0.1`**, with a TLS reverse proxy (Traefik/Caddy/nginx) terminating in front. Binding `0.0.0.0` directly puts an unencrypted control plane on the LAN: enrollment bearer tokens and the setup token travel in the clear.

**The installer does not download the binary in this version.** Put the `warphold` binary at `~/.local/bin/warphold` (or `/usr/local/bin/warphold` for `--scope system`) on the device first and `enroll.sh` will use it as-is; the download from `GET /dl/warphold-linux-<arch>` arrives in Plan 2, so without a preinstalled binary the script fails loudly instead of enrolling. With the binary in place the script enrolls it against the token and installs a `systemd --user` unit (`warphold agent install --scope user`) so the agent runs and polls automatically.

**The Fleet admin can decrypt every enrolled device's backups.** Fleet holds the admin key for every target it provisions, so it can run maintenance and generate recovery kits on agents' behalf — for a family or personal fleet that's the point, but it means Fleet's admin passphrase is the one secret that must never leak. And the per-agent B2 *writer* key is not as harmless as "writer" suggests: Kopia's B2 backend implements blob deletion as `b2_hide_file`, which needs only `writeFiles`, so a compromised agent can hide every blob under its own prefix and make its repository look empty. Object Lock is what makes that recoverable — the retained versions are still there and can be un-hidden — not the key's permission set.

### Operations notes
- **Activation is one-shot.** If activation fails half-way (key file or DB present but unusable), delete `<state dir>/seal.key` and `<state dir>/fleet.db` before retrying. WarpHold refuses to overwrite an existing key file on purpose: that file unlocks every escrowed repository password.
- **Electron desktop app (`app/`)** is upstream KopiaUI packaging and is not built or shipped by WarpHold; the WarpHold tray (`warphold agent tray`) replaces it on Linux.

## About the Kopia engine

Pick the Cloud Storage Provider You Want
---

Kopia supports saving your [encrypted](https://kopia.io/docs/features/#user-controlled-end-to-end-encryption) and [compressed](https://kopia.io/docs/features/#compression) snapshots to all of the following [storage locations](https://kopia.io/docs/features/#save-snapshots-to-cloud-network-or-local-storage):

* **Amazon S3** and any **cloud storage that is compatible with S3**
* **Azure Blob Storage**
* **Backblaze B2**
* **Google Cloud Storage**
* Any remote server or cloud storage that supports **WebDAV**
* Any remote server or cloud storage that supports **SFTP**
* Some of the cloud storage options supported by **Rclone**
  * Requires you to download and setup Rclone in addition to Kopia, but after that Kopia manages/runs Rclone for you
  * Rclone support is experimental: not all the cloud storage products supported by Rclone have been tested to work with Kopia, and some may not work with Kopia; Kopia has been tested to work with **Dropbox**, **OneDrive**, and **Google Drive** through Rclone
* Your local machine and any network-attached storage or server
* Your own server by setting up a [Kopia Repository Server](https://kopia.io/docs/repository-server/)

And Kopia uses [data deduplication](https://kopia.io/docs/features/#backup-files-and-directories-using-snapshots) to save you money! Read the [repositories help page](https://kopia.io/docs/repositories/) for more information on supported storage locations.

With Kopia you are in full control of where to store your snapshots, that is, you pick the storage provider you want to use. You must provision and pay for the storage provider for whatever storage locations you want to use, and then tell Kopia what those storage locations are. You can even use multiple storage locations for different backup repositories if you want. Kopia also supports backing up multiple machines to the same storage location.

Kopia in Action
---

Using Kopia via command-line interface:

[![asciicast](https://asciinema.org/a/ykx6uzEhKY3451fWEnX9nm9uo.svg)](https://asciinema.org/a/ykx6uzEhKY3451fWEnX9nm9uo)

Using Kopia via graphical user interface (note: the video is of an older version of Kopia and the interface is different in the current version of Kopia, but the main principles of the interface are the same):

[![Kopia UI Tutorial](https://img.youtube.com/vi/sHJjSpasWIo/0.jpg)](https://www.youtube.com/watch?v=sHJjSpasWIo)

Getting Started
---
See [Kopia Documentation](https://kopia.io/docs/) for more information. Also check out the [users forum](https://kopia.discourse.group).

Licensing
---
Kopia is licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) for the full license text.

Building Kopia
---
See [Build Infrastructure](BUILD.md) for more information on building Kopia and working with the source code.

Contribution Guidelines
---
Kopia is open source. For more information see the [Contribution Guidelines](https://kopia.io/docs/contribution-guidelines/).

Reporting Security Issues
---
If you find a security issue you'd like to disclose privately, please contact `security@kopia.io`.

[![Netlify Status](https://api.netlify.com/api/v1/badges/6b5c1fe4-a0da-4e7e-939b-ff1105251985/deploy-status)](https://app.netlify.com/sites/kopia/deploys)
