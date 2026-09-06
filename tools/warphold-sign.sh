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

# nullglob: a release with no RPMs would otherwise iterate once over the
# literal "dist/*rpm" and fail the whole signing step under set -e.
shopt -s nullglob

for f in dist/*rpm; do
  # add signature to RPMs, using the WarpHold key instead of upstream's "Kopia Builder"
  rpm --define "%_gpg_name ${WARPHOLD_SIGNING_KEY_ID}" \
      --define "%__gpg_sign_cmd ${gpg_sign_cmd}" \
      --addsign "$f" 3< <(printf '%s' "$WARPHOLD_SIGNING_PASSPHRASE")
done

# before signing checksums.txt, regenerate it since we've just signed some RPMs.
# Read line by line rather than word-splitting one string: an artifact name
# containing a space would otherwise be checksummed as two missing files.
(
  cd dist
  # "<hash><space><space-or-*><name>": drop the hash and the two-character
  # separator only, so a name containing a space survives intact. cut -f 2-
  # left a leading space or "*" on every name, which sha256sum then read as
  # part of a nonexistent file.
  sed -E 's/^[0-9a-fA-F]+ [ *]//' checksums.txt > .names
  : > checksums.new
  while IFS= read -r name; do
    [ -n "$name" ] || continue
    sha256sum -- "$name" >> checksums.new
  done < .names
  mv checksums.new checksums.txt
  rm -f .names
)

gpg --batch --pinentry-mode loopback --passphrase-fd 0 \
    --local-user "$WARPHOLD_SIGNING_KEY_ID" \
    --output dist/checksums.txt.sig --detach-sig dist/checksums.txt \
    <<<"$WARPHOLD_SIGNING_PASSPHRASE"
