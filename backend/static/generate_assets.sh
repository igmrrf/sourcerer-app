#!/usr/bin/env bash
# Regenerates the icon set and the social share card from favicon.svg.
#
# The outputs are committed, so this only needs running when the mark or the
# wordmark changes. Requires ImageMagick 7 (`magick`).
#
#   cd backend/static && ./generate_assets.sh
set -euo pipefail

cd "$(dirname "$0")"

command -v magick >/dev/null || { echo "ImageMagick 7 (magick) is required" >&2; exit 1; }

# ImageMagick's built-in SVG renderer drops stroked paths, which is why
# favicon.svg is drawn with filled rects and circles only. Keep it that way.
magick -background none favicon.svg -resize 192x192 -depth 8 icon-192.png
magick -background none favicon.svg -resize 512x512 -depth 8 icon-512.png

# Apple's touch icon is composited on an opaque background by iOS anyway;
# flattening here keeps the corners from going grey.
magick -background none favicon.svg -resize 180x180 -depth 8 \
  -background '#0d1117' -alpha remove -alpha off apple-touch-icon.png

magick -background none favicon.svg -define icon:auto-resize=48,32,16 favicon.ico

# Social share card. Avenir Next stands in for Plus Jakarta Sans, which is not
# installed system-wide; the faux-bold stroke matches the wordmark's weight.
FONT="/System/Library/Fonts/Avenir Next.ttc"
magick -size 1200x630 xc:'#0d1117' \
  \( icon-512.png -resize 132x132 \) -gravity northwest -geometry +88+72 -composite \
  -font "$FONT" -fill white -stroke white -strokewidth 1.4 -pointsize 102 \
  -annotate +86+300 'Sourcerer' \
  -stroke none -fill '#9aa2b1' -pointsize 36 \
  -annotate +90+432 'Engineering profiles from git history' \
  -fill '#0e9f6e' -draw 'roundrectangle 88,506 742,536 8,8' \
  -fill '#e5484d' -draw 'roundrectangle 754,506 986,536 8,8' \
  og.png

echo "Regenerated: icon-192.png icon-512.png apple-touch-icon.png favicon.ico og.png"
