# Spike — what Kopia deletes and overwrites on append-only S3 (2026-09-02)

Plan 3 Task 1 / decision D4. The result lives in **[`docs/RECONCILE-append-only.md`](../../RECONCILE-append-only.md)**
alongside the other reconcile notes; the harness is [`scripts/spike-append-only/main.go`](../../../scripts/spike-append-only/main.go).

**One-line answer:** stock Kopia deletes exactly one blob class — `s…` session markers, one
per write session — and overwrites exactly one key, `kopia.maintenance`, which disappears
entirely once the repository's maintenance owner is the Fleet rather than the device. So
`allowDelete = ["^s[0-9a-f]{16,}"]`, `allowOverwrite = []`.
