#!/usr/bin/env bash
# Re-record the README GIFs. Needs vhs (brew install vhs) and TYPESAFE_API_KEY in .env.
#   docs/demo/record.sh            # every tape
#   docs/demo/record.sh tui        # just tui.tape
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
bin="$(mktemp -d)"
trap 'rm -rf "$bin"' EXIT
go build -o "$bin/hunch" "$root/cmd/hunch"
export PATH="$bin:$PATH"
cd "$root"
tapes=("$@"); [ ${#tapes[@]} -eq 0 ] && tapes=(run test tui)
for t in "${tapes[@]}"; do
  echo "recording $t"
  vhs "$here/$t.tape"
done
