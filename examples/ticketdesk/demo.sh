#!/usr/bin/env bash
# Runs the severity check for real on this machine:
#   ticketdesk (a toy ticket system) ─webhook─▶ hunch serve ─▶ Jev decides
#   the severity and the facts behind it, a local Ollama model writes the
#   note from those facts, Jev checks it ─▶ severity lowered + comment.
#
# Needs: Go, a Jev key in .env (TYPESAFE_API_KEY), and Ollama with the note
# model (ollama pull qwen2.5:1.5b; ollama serve).
#
#   examples/ticketdesk/demo.sh
#
# Then open the recorded runs:  go run ./cmd/hunch tui examples/severity.yaml --runs .demo/runs
set -euo pipefail
cd "$(dirname "$0")/../.."

export HOOK_TOKEN=${HOOK_TOKEN:-demo-secret}
export TICKETS_TOKEN=$HOOK_TOKEN
export TICKETS_API=http://127.0.0.1:8090

mkdir -p .demo
go build -o .demo/hunch ./cmd/hunch
go build -o .demo/ticketdesk ./examples/ticketdesk

.demo/hunch serve examples/severity.yaml --addr 127.0.0.1:8080 --token-env HOOK_TOKEN \
  --dedupe-key '{{ticket_id}}' --dedupe-store .demo/dedupe --trace .demo/runs 2>.demo/hunch.log &
HUNCH=$!
.demo/ticketdesk --addr 127.0.0.1:8090 --webhook http://127.0.0.1:8080/ --token-env HOOK_TOKEN 2>.demo/desk.log &
DESK=$!
trap 'kill $HUNCH $DESK 2>/dev/null' EXIT
sleep 1.5

auth="Authorization: Bearer $HOOK_TOKEN"
new() { curl -s -H "$auth" -d "$1" "$TICKETS_API/tickets" >/dev/null; }
new '{"severity":"sev2","title":"Logo slightly blurry on the settings page","description":"The company logo on the settings page looks a bit blurry on retina screens. Everything works.","request":"Could you replace it with a sharper image?"}'
new '{"severity":"sev2","title":"Checkout fails for all EU customers","description":"Since 09:00 every checkout in the EU region returns an error. No customer can pay. We have no way around it.","request":"Please fix urgently, we are losing all EU sales."}'
new '{"severity":"sev2.5","title":"How do I change my invoice email?","description":"I want invoices to go to finance@ instead of my own address. I could not find the setting.","request":"Where is this setting?"}'
new '{"severity":"sev2.5","title":"CSV export times out for large accounts","description":"Exports over 50k rows time out for about 20 customers. Exporting by date range works as a workaround.","request":"Please make large exports work."}'
new '{"severity":"sev1","title":"Whole platform down","description":"Nothing loads for anyone.","request":"Help."}'

echo "5 tickets created; waiting for hunch..."
# Wait until hunch has answered all five webhooks (a model's first call can
# take a while as it loads), up to two minutes.
for _ in $(seq 120); do
  [ "$(grep -c 'webhook →' .demo/desk.log || true)" -ge 5 ] && break
  sleep 1
done
curl -s -H "$auth" "$TICKETS_API/tickets" | python3 -c '
import json, sys
for t in json.load(sys.stdin):
    print("#%s [%s] %s" % (t["id"], t["severity"], t["title"]))
    print("    " + " · ".join(t["history"]))
    for c in t["comments"]:
        print("    comment: " + c["body"])
'
echo
echo "logs: .demo/hunch.log .demo/desk.log · replay: go run ./cmd/hunch tui examples/severity.yaml --runs .demo/runs"
