#!/usr/bin/env bash
# Retake the README screenshot (docs/demo/tui-<date>.png).
# Needs vhs (brew install vhs) and TYPESAFE_API_KEY in .env. The date in the
# name keeps GitHub from showing an old one from its cache.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
bin="$(mktemp -d)"
go build -o "$bin/hunch" "$root/cmd/hunch"
export PATH="$bin:$PATH"
cd "$root"
rm -rf examples/hello.runs
trap 'rm -rf "$bin" examples/hello.runs' EXIT
mkdir -p .demo/rec
vhs "$here/screenshot.tape"
png="tui-$(date +%F-%H%M).png"
rm -f "$here"/tui-*.png
cp .demo/rec/screenshot.png "$here/$png"
sed -i.bak -E "s#docs/demo/tui-[0-9-]+\\.png#docs/demo/$png#" README.md && rm README.md.bak
echo "docs/demo/$png"
