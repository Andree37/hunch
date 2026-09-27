#!/usr/bin/env bash
# Runs network-team triage for real on this machine:
#   ticketdesk (a toy ticket system) ─webhook─▶ hunch serve ─▶ Jev decides
#   whose ticket it is (rerouting it if not ours), then its severity and the
#   facts behind it; a local Ollama model writes the note from those facts,
#   Jev checks it ─▶ rerouted, or severity lowered + comment.
#
# Needs: Go, a Jev key in .env (TYPESAFE_API_KEY), and Ollama with the note
# model (ollama pull qwen2.5:1.5b; ollama serve).
#
#   examples/ticketdesk/demo.sh
#
# Then open the recorded runs:  go run ./cmd/hunch tui examples/triage.yaml
# (runs are recorded next to the flow, in examples/triage.runs/)
set -euo pipefail
cd "$(dirname "$0")/../.."

export HOOK_TOKEN=${HOOK_TOKEN:-demo-secret}
export TICKETS_TOKEN=$HOOK_TOKEN
export TICKETS_API=http://127.0.0.1:8090

mkdir -p .demo
go build -o .demo/hunch ./cmd/hunch
go build -o .demo/ticketdesk ./examples/ticketdesk

.demo/hunch serve examples/triage.yaml --addr 127.0.0.1:8080 --token-env HOOK_TOKEN \
  --dedupe-key '{{ticket_id}}' --dedupe-store .demo/dedupe 2>.demo/hunch.log &
HUNCH=$!
.demo/ticketdesk --addr 127.0.0.1:8090 --webhook http://127.0.0.1:8080/ --token-env HOOK_TOKEN 2>.demo/desk.log &
DESK=$!
trap 'kill $HUNCH $DESK 2>/dev/null' EXIT
sleep 1.5

auth="Authorization: Bearer $HOOK_TOKEN"
new() { curl -s -H "$auth" -d "$1" "$TICKETS_API/tickets" >/dev/null; }
new '{"severity":"sev2","title":"My computer is down","description":"My laptop is on and works, but it has had no internet since this morning. The cable is plugged in and colleagues next to me are fine. I am working from my phone hotspot for now.","request":"Please get my laptop back online."}'
new '{"severity":"sev2","title":"My computer is down","description":"When I press the power button nothing happens, no lights, no fan. It was fine yesterday. Charger is plugged in.","request":"I need a working computer."}'
new '{"severity":"sev2","title":"No Wi-Fi on the 3rd floor","description":"Since 10:00 nobody on the 3rd floor can connect to Wi-Fi. That is about 40 people in finance. There are no wired ports up here.","request":"Please restore the Wi-Fi, finance cannot work."}'
new '{"severity":"sev2","title":"VPN keeps dropping","description":"Several people in the remote sales team get disconnected from the VPN about once an hour. Reconnecting works straight away.","request":"Please make the VPN stable."}'
new '{"severity":"sev2","title":"Cannot log in, password expired","description":"My password expired over the weekend and now my account is locked. I cannot sign in to anything.","request":"Please unlock my account."}'
new '{"severity":"sev1","title":"Whole Lisbon office has no internet","description":"Nobody in the Lisbon office can reach the internet or the VPN since 08:30.","request":"Urgent, the office is down."}'

echo "6 tickets created; waiting for hunch..."
# Wait until hunch has answered all six webhooks (a model's first call can
# take a while as it loads), up to two minutes.
for _ in $(seq 120); do
  [ "$(grep -c 'webhook →' .demo/desk.log || true)" -ge 6 ] && break
  sleep 1
done
curl -s -H "$auth" "$TICKETS_API/tickets" | python3 -c '
import json, sys
for t in json.load(sys.stdin):
    print("#%s [%s · %s] %s" % (t["id"], t["severity"], t["team"], t["title"]))
    print("    " + " · ".join(t["history"]))
    for c in t["comments"]:
        print("    comment: " + c["body"])
'
echo
echo "logs: .demo/hunch.log .demo/desk.log · replay: go run ./cmd/hunch tui examples/triage.yaml"
