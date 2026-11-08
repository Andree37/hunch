#!/usr/bin/env bash
# Re-record the README GIFs.
#   docs/demo/record.sh            # every tape
#   docs/demo/record.sh tui        # just tui.tape
#
# Needs vhs (brew install vhs), ffmpeg, python3 with Pillow, and
# TYPESAFE_API_KEY in .env. Each tape records a video into .demo/rec/;
# annotate.py then zooms and captions it using <tape>.json and writes
# docs/demo/hello-<tape>-<date>-<time>.gif. The date makes every recording a new
# file name, so GitHub can't keep showing an old one from its cache; the old
# GIF is removed and README.md is pointed at the new one.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
bin="$(mktemp -d)"
go build -o "$bin/hunch" "$root/cmd/hunch"
export PATH="$bin:$PATH"
cd "$root"
# runs recorded by the demo would make the TUI open on runs instead of tests
rm -rf examples/hello.runs
trap 'rm -rf "$bin" examples/hello.runs' EXIT
mkdir -p .demo/rec
day="$(date +%F-%H%M)"
tapes=("$@"); [ ${#tapes[@]} -eq 0 ] && tapes=(tui test run)
for t in "${tapes[@]}"; do
  echo "recording $t"
  vhs "$here/$t.tape"
  gif="hello-$t-$day.gif"
  python3 "$here/annotate.py" ".demo/rec/$t.mp4" "$here/$t.json" "$here/$gif"
  for old in "$here"/hello-"$t"-*.gif; do
    [ "$(basename "$old")" = "$gif" ] || rm -f "$old"
  done
  sed -i.bak -E "s#docs/demo/hello-$t-[0-9-]+\.gif#docs/demo/$gif#" README.md && rm README.md.bak
done
