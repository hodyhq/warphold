#!/bin/sh
# Regenerates the tray icons from icons/warphold.svg: the ember mark recolored
# per tone, rasterized at 22 px (panel) and 44 px (HiDPI panel).
#
# Run from the repository root: agent/tray/icons/gen.sh
# Requires rsvg-convert (librsvg) or ImageMagick's magick/convert.
set -eu

src=icons/warphold.svg
out=agent/tray/icons

[ -f "$src" ] || { echo "run me from the repository root: $src not found" >&2; exit 1; }

if command -v rsvg-convert >/dev/null 2>&1; then
  render() { rsvg-convert -w "$2" -h "$2" -o "$3" "$1"; }
elif command -v magick >/dev/null 2>&1; then
  render() { magick -background none "$1" -resize "$2x$2" "$3"; }
elif command -v convert >/dev/null 2>&1; then
  render() { convert -background none "$1" -resize "$2x$2" "$3"; }
else
  echo "need rsvg-convert or ImageMagick" >&2
  exit 1
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# tone:color - good is green, ember the brand orange (uploading), warn amber,
# bad red, dim grey (no engine running).
for pair in good:#3FB950 ember:#FF6A1A warn:#D29922 bad:#F85149 dim:#6E7681; do
  tone=${pair%%:*}
  color=${pair#*:}
  sed "s/#FF6A1A/$color/" "$src" >"$tmp/$tone.svg"
  for size in 22 44; do
    render "$tmp/$tone.svg" "$size" "$out/$tone-$size.png"
  done
done

echo "wrote $out/{good,ember,warn,bad,dim}-{22,44}.png"
