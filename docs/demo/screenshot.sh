#!/usr/bin/env bash
# Retake the README screenshot (docs/demo/tui-<date>.png).
# Needs vhs (brew install vhs) and a hunch.yaml with jev. The date in the
# name keeps GitHub from showing an old one from its cache.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
bin="$(mktemp -d)"
go build -o "$bin/hunch" "$root/cmd/hunch"
export PATH="$bin:$PATH"
cd "$root"
# Start from no recorded runs, and put yours back afterwards.
saved="$bin/hello.runs"
[ -e examples/hello.runs ] && mv examples/hello.runs "$saved"
restore() {
  rm -rf examples/hello.runs
  [ -e "$saved" ] && mv "$saved" examples/hello.runs
  rm -rf "$bin"
}
trap restore EXIT
mkdir -p .demo/rec
vhs "$here/screenshot.tape"
png="tui-$(date +%F-%H%M).png"
rm -f "$here"/tui-*.png
cp .demo/rec/screenshot.png "$here/$png"
sed -i.bak -E "s#docs/demo/tui-[0-9-]+\\.png#docs/demo/$png#" README.md && rm README.md.bak
echo "docs/demo/$png"
