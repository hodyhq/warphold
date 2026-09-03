#!/bin/bash
# warphold: signing helper invoked by .goreleaser.yml's `signs.cmd`.
# tools/sign.sh is upstream and untouched (unrelated "Kopia Builder" key
# baked into rpm macros with no way to parameterize it without editing an
# upstream file); this replacement reads the WarpHold key id and passphrase
# from the environment instead. Never `set -x` — the passphrase only ever
# flows through gpg's --passphrase-fd, never argv, never echoed.
set -euo pipefail

: "${WARPHOLD_SIGNING_KEY_ID:?WARPHOLD_SIGNING_KEY_ID is required (set by the Import signing key workflow step)}"
: "${WARPHOLD_SIGNING_PASSPHRASE:?WARPHOLD_SIGNING_PASSPHRASE is required}"

gpg_sign_cmd='%{__gpg} gpg --batch --pinentry-mode loopback --passphrase-fd 3 --no-verbose --no-armor -u "%{_gpg_name}" -sbo %{__signature_filename} %{__plaintext_filename}'

for f in dist/*rpm; do
  # add signature to RPMs, using the WarpHold key instead of upstream's "Kopia Builder"
  rpm --define "%_gpg_name ${WARPHOLD_SIGNING_KEY_ID}" \
      --define "%__gpg_sign_cmd ${gpg_sign_cmd}" \
      --addsign "$f" 3< <(printf '%s' "$WARPHOLD_SIGNING_PASSPHRASE")
done

# before signing checksums.txt, regenerate it since we've just signed some RPMs.
filenames=$(cut -f 2- -d " " dist/checksums.txt)
(cd dist && sha256sum $filenames > checksums.txt)

gpg --batch --pinentry-mode loopback --passphrase-fd 0 \
    --local-user "$WARPHOLD_SIGNING_KEY_ID" \
    --output dist/checksums.txt.sig --detach-sig dist/checksums.txt \
    <<<"$WARPHOLD_SIGNING_PASSPHRASE"
