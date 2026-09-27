# hunch

Decision flows with typed, calibrated answers.

A flow is a YAML graph. **Decision nodes** ask a backend a typed question
(`bool`, `choice`, `score`, or several at once) and branch on the answer and
its confidence. **Action nodes** do things. Every result lands in shared state,
so later nodes build on earlier ones via `{{node.field}}` refs.

Backends are swappable: Jev, local models, or the built-in deterministic mock.

```sh
go run ./cmd/hunch validate examples/inbox.yaml
go run ./cmd/hunch run examples/inbox.yaml --set sender=boss --set message="Can we sync Thursday?"
```

## Flow file

```yaml
name: example
threshold: 0.6          # below this confidence, `unsure` routes win (default 0.6)
backends:
  default: mock
  mock: {kind: mock, seed: demo}
nodes:
  gate:                 # the first node is the start (or set `start:`)
    bool: "Does {{who}} need an answer?"
    then: {yes: kind, no: skip, unsure: ask}

  kind:
    choice: "What do they want?"
    options: [meeting, info, sales]
    then: {sales: skip, _: size}      # `_` is the fallback

  size:
    questions:                        # several questions, one call
      urgency: {score: "How urgent?", scale: 1-5}
      formal:  {bool: "Formal reply?"}
    then: done

  done: {action: log, message: "{{kind.answer}} / urgency {{size.urgency.level}}"}
  skip: {action: log, message: skipped}
  ask:
    action: shell
    run: say "hi {{who}}"             # refs are escaped for whatever quotes they sit in
```

### Rules

A `switch` node routes on a value with plain code, no model involved. Use it
for fixed rules; save the model for judgement.

```yaml
by_plan:
  switch: "{{account.plan}}"
  then: {team: review, enterprise: review, _: done}
```

### Shared definitions and outputs

The flow's `state:` holds constants every node can use, so a definition is
written once. They're sent to deciders along with the input, and are never
asked for as inputs. Refs work in answer descriptions and score levels too,
and keys may contain dots (`{{versions.v2.5}}`).

```yaml
state:
  kinds:
    invoice: Asks for a payment, with an amount due
    receipt: Confirms a payment that already happened
nodes:
  kind:
    choice: "What kind of document is this? {{doc.text}}"
    options: {invoice: "{{kinds.invoice}}", receipt: "{{kinds.receipt}}"}
```

Once an `output` node has run, later nodes can read everything set so far as
`{{outputs.name}}`, whichever branch set it.

### What a model sees

By default a decision is shown the whole state: every input, constant and
earlier answer. `sees:` limits it to the paths it needs, which saves tokens
and keeps sensitive fields away from the model:

```yaml
check:
  choice: "Which kind fits?"
  sees: [record.title, record.body, kinds]   # quote optional ones: "record.notes?"
```

`sees: []` shows the question alone. Paths are checked like refs, and the
TUI's Node pane lists exactly what was sent.

### Actions

Decisions steer; actions do the work. Every action's result lands in state
under its node name, so later nodes can use it.

| Action | Fields | Result |
|---|---|---|
| `log` (shown as SAY) | `message` | `message` |
| `shell` | `run`, `timeout` (s, default 60) | `stdout`, `exit_code` |
| `llm` | `prompt`, `system`, `max_tokens`, `backend` | `text` |
| `http` | `url`, `method` (default POST), `headers`, `body`, `timeout` (s, default 30), `retries` (default 2), `idempotency_key` | `status`, `body` (parsed if JSON) |
| `output` | `set: {name: value}` | the values; also the flow's **outputs** |

```yaml
backends:
  default: jev          # decides
  writer: claude        # writes, for llm nodes
  jev: {kind: jev}
  claude: {kind: anthropic, model: claude-opus-5}

nodes:
  draft:
    action: llm
    system: You write short, friendly first replies.
    prompt: "Reply to: {{message}}"
    then: ok
  ok:
    bool: "Safe to send without a person? {{draft.text}}"
    then: {yes: send, no: hold}
  send:
    action: http
    url: "{{post_url}}"
    headers: {Authorization: "Bearer $API_TOKEN"}   # $VARS come from the environment / .env
    body: {reply: "{{draft.text}}"}
    then: sent
  sent: {action: output, set: {action: sent}}
  hold: {action: output, set: {action: needs_human, reply: "{{draft.text}}"}}
```

- **Sending is a decision.** Put the "should this go out?" question before
  the `http` node, as above.
- **Dry runs.** `hunch test` and the TUI never write: `http` nodes that
  POST/PUT/PATCH/DELETE record the request they would make. GETs still run,
  since they only read and later nodes need the data. Use `hunch test --live` or `L` in the TUI to send
  for real. `hunch run` sends unless given `--dry-run`.
- **Secrets** go in `$VARS` in `url` and `headers`, read from the environment,
  so they never enter state or any model's prompt. Refs in a URL are escaped.
- A `body` map is sent as JSON; a ref that is the whole value keeps its type.
- **Retries** never risk doing something twice: GET/PUT/DELETE retry network
  errors, 429 and 5xx; POST/PATCH only retry 429 and 503 (not processed)
  unless you give an `idempotency_key` (sent as `Idempotency-Key`, e.g.
  `"comment-{{record.id}}"`), which lets the receiving API drop duplicates.
- `output` values are what your code reads: `hunch run --json` prints the
  final state, and each run prints its outputs. Test cases can expect them:
  `sent.action: sent`.

See `examples/respond.yaml` for a full flow: decide, draft, judge, then send
or hand to a person.

### Inputs

Declare what each input accepts. This holds no values (those come from test
cases, `--set`, or the TUI); it types them and gives the TUI a picker for
choices and yes/no.

```yaml
inputs:
  channel: [email, slack, sms]            # choice (short form)
  vip: {type: bool}                       # yes / no
  count: {type: number, min: 1, max: 10}
  message: {type: text, description: The message itself}
```

Undeclared inputs still work as free-form values. Bad values are refused
before a run: `--set channel: "fax" is not one of email, slack, sms`.

### Descriptions

Backends judge better when they know what each answer means:

```yaml
bug:
  bool: "Is this a defect?"
  criteria: {yes: "Broken or unexpected behavior", no: "A question or feature request"}
team:
  choice: "Which team owns this?"
  options: {payments: "Checkout and billing", frontend: "Rendering and layout"}
urgency:
  score: "How urgent?"
  levels: ["Can wait", "This week", "Blocking revenue now"]   # scale 1-3
```

### Refs

`{{node.field}}` reads a node's result; `{{name}}` with no node of that name
is an input. The validator warns when some path reaches a
node without running the node a ref points at. If that's intended (e.g. the
first pass of a loop), mark the ref optional with `{{node.field?}}` and it
renders empty when missing.

### State shape

| Node | Available as |
|---|---|
| `bool` | `answer` (true/false), `p` (probability of yes), `confidence` |
| `choice` | `answer` (option), `probs`, `confidence` |
| `score` | `answer` (expected value), `level` (rounded), `probs`, `confidence` |
| `questions` | one entry per question, e.g. `size.urgency.level` |
| `log` | `message` |
| `shell` | `stdout`, `exit_code` |

### Score branches

`">=4"`, `">3"`, `"<=2"`, `"<3"`, `"2-4"` (inclusive), or `"3"` (rounds to 3).
Score answers are expected values, so a run can land between levels; the
validator warns about gaps.

## Serving

`hunch serve FLOW` runs the flow for every `POST /`: the JSON body is the
input, the reply is the path taken and the outputs. Point any webhook at it
(a form submitted, a record created, a message received).

```sh
hunch serve examples/respond.yaml --backend jev --token-env HOOK_TOKEN
curl -H "Authorization: Bearer $HOOK_TOKEN" \
  -d '{"channel": "chat", "message": "How do I reset my password?", "post_url": "https://..."}' \
  http://127.0.0.1:8080/
# {"path": ["needs_reply", "kind", "draft", ...], "outputs": {"action": "sent", ...}, "cost_usd": 0.00004}
```

- Listens on `127.0.0.1:8080` unless `--addr` says otherwise.
- `--token-env VAR` requires `Authorization: Bearer <$VAR>`.
- Inputs are checked like everywhere else; bad or missing ones get a 400.
- Live by default; `--dry-run` records writes instead of sending them.
- A failed run returns 500 with the error and `failed_at` node.
- `--trace` records every run (see [Recording and replaying runs](#recording-and-replaying-runs));
  `GET /healthz` for checks.
- **Duplicates**: webhook senders retry. With `--dedupe-key '{{record.id}}'`,
  or when the sender sends an `Idempotency-Key` header, a repeat of a
  finished run gets that run's reply (marked `"duplicate": true`) without
  running again, and a repeat of one still running gets 409. Failed runs
  aren't remembered, so a retry runs them again. Remembered for
  `--dedupe-ttl` (24h), in memory unless you give `--dedupe-store`:
  - `--dedupe-store ./dedupe/` or `--dedupe-store s3://bucket/dedupe/` keeps
    one object per key, so the memory survives restarts and is **shared by
    every serve using the same store**: several tasks behind a load balancer
    run each delivery exactly once. Claims are atomic (create-if-absent on
    disk, S3 conditional writes).
  - A claim left "running" by a serve that died is taken over after
    `--timeout` + 1 minute.
  - If the store can't be reached, the webhook gets 503 so the sender retries,
    rather than risk running it twice. Set an S3 lifecycle rule on the prefix
    to clean up old entries.
- **Load**: at most `--max-concurrent` (4) runs at once; others wait up to 30s,
  then get 503 with `Retry-After`.
- **Runs finish** even if the sender hangs up, bounded by `--timeout` (5m), so
  a run never stops between two writes.
- **Edits** to the flow file are picked up on the next request; a broken edit
  is logged and the last good flow keeps serving.

## Recording and replaying runs

`--trace` on `hunch run` or `hunch serve` records every run: its input, each
step (question as asked, what the model saw, probabilities, route, cost), and
how it ended. It goes where you point it:

| `--trace` | Stored as |
|---|---|
| `runs.jsonl` | one file, appended to, rolling over at `--trace-max-mb` (100) and keeping 3 old files |
| `./runs/` | one file per run, named by start time: `2026-09-27/18-17-19.123-<id>.jsonl` |
| `s3://bucket/runs/` | the same, as S3 objects (credentials and region from the usual AWS chain; `?region=` overrides) |

One object per run means several serve tasks never write over each other.
Open the recordings in the TUI from any of the three:

```sh
hunch serve examples/respond.yaml --trace s3://my-bucket/runs/   # production
hunch tui examples/respond.yaml --runs s3://my-bucket/runs/      # look at what happened
```

- Pane 2 lists the recorded runs, newest first (`t` switches to test cases);
  it updates while serve keeps recording (a file on change; a directory or
  S3 every 10s, in the background).
- Enter loads a run into the graph and path views, with its input in pane 4.
- `s` replays it one step at a time, exactly as it happened; `r` runs the same
  input again live, e.g. after changing the flow, to see if it now goes the
  way it should.
- `n` saves it as a test case: its input, the http replies it got (as fakes,
  so it runs offline) and every answer it gave as `expect:`. Fix the answers
  it got wrong and the test fails until the flow gets them right. `n` does the
  same after any finished run in the TUI; with no run of the current inputs on
  screen it saves just the inputs.

## Tuning thresholds

`hunch tune FLOW` shows, for each node that routes on confidence, what every
threshold would do with the answers already collected: how many go through
automatically, how many go to `unsure`, and how many would be confident but
wrong.

```sh
hunch tune examples/severity.yaml --runs runs.jsonl --backend jev
```

- Recorded runs (`--runs`) are real traffic but don't say what's right; test
  cases are run once and do. `--no-tests` uses recordings only.
- It suggests the lowest threshold with no confident wrong answer among the
  known ones, and warns when there are fewer than 10 of those to go on.
- Each node is judged on the runs that reached it; a changed threshold
  upstream changes which runs reach it.

## Try it for real, locally

`examples/ticketdesk` is a toy ticket system that sends a webhook when a
ticket is created and accepts severity changes and comments. The demo wires it
to `hunch serve` running `examples/severity.yaml`:

1. Jev decides the severity (it can only stay or go down).
2. Jev establishes the facts the SLA cares about in one multi-question call:
   core feature down? workaround? how many users? what kind of ticket?
3. A second model, a local Ollama model (the `notes` backend), writes the note
   from the SLA, the decision and those facts.
4. Jev checks the note before it goes on the ticket; if it's off, a plain note
   built from the facts is posted instead.

```sh
ollama pull qwen2.5:1.5b && ollama serve   # the note model
examples/ticketdesk/demo.sh                # needs TYPESAFE_API_KEY in .env
```

It creates five tickets and prints what happened to each:

```
#1 [sev3] Logo slightly blurry on the settings page
    created as sev2 · severity sev2 → sev3 · comment added
    comment: Severity moved from sev2 to sev3 under our SLA. Kind: cosmetic · core feature down: no · workaround: no · users affected: some.
#2 [sev2] Checkout fails for all EU customers
    created as sev2
#3 [sev3] How do I change my invoice email?
    created as sev2.5 · severity sev2.5 → sev3 · comment added
#4 [sev2.5] CSV export times out for large accounts
#5 [sev1] Whole platform down               (out of scope: only sev2 and sev2.5 are checked)
```

Replay any of them step by step: `go run ./cmd/hunch tui examples/severity.yaml --runs .demo/runs`.

## Examples

Each is a different shape of the same building blocks; none is special.

| Flow | Shows |
|---|---|
| `examples/inbox.yaml` | Chained decisions: gate, classify, multi-question, score, route on confidence. |
| `examples/respond.yaml` | Decide, write with a model, judge the result, then send it or hand it to a person. |
| `examples/severity.yaml` | A webhook-driven check: a rule limits scope, a model re-classifies (only downwards), a writer explains, the source record is updated. |

Each has a `<flow>.tests/` folder you can run with `hunch test`.

## Keys

Put API keys in `.env` (gitignored; see `.env.example`). Variables already set in
your environment take precedence.

## Backends

Any backend can make decisions. Chat-model backends can also write text for
`llm` nodes. Chat models report their own confidence, so their probabilities
aren't calibrated the way Jev's are.

| Kind | Decides | Writes | Options |
|---|---|---|---|
| `mock` | ✓ | ✓ | Deterministic, offline. `answers: {node: value}` forces answers. |
| `jev` | ✓ | | TypeSafe's Jev via `provider: typesafe` (`$TYPESAFE_API_KEY`) or `openrouter` (`$OPENROUTER_API_KEY`). |
| `openai` | ✓ | ✓ | Any OpenAI-compatible server: OpenAI, Ollama (`base_url: http://localhost:11434/v1`), OpenRouter, vLLM, LM Studio. `model`, `base_url`, `api_key_env` (default `OPENAI_API_KEY`). |
| `anthropic` | ✓ | ✓ | Claude via the Anthropic API. `model` (e.g. `claude-opus-5`), `api_key_env` (default `ANTHROPIC_API_KEY`), `base_url`. |
| `bedrock` | ✓ | ✓ | Any Amazon Bedrock model via the Converse API. `model` (model or inference profile ID), `region`, `profile`; AWS credentials from the usual chain. |

Routing on confidence (`unsure`, `threshold`) is most meaningful with Jev.
The validator warns when a chat model's self-reported confidence drives a
route, and the TUI marks those numbers `self-reported`; check such
thresholds against your test cases.

Swap models per run without editing the flow: `--backend` picks who decides
and `--writer` who writes, on `run`, `test`, `tune`, `serve` and `tui`.

```sh
hunch test examples/respond.yaml --backend jev                    # 3/3
hunch test examples/respond.yaml --backend ollama --writer ollama # a 0.5B local model: 1/3
```

For decisions, `openai` backends send a JSON schema the server must follow
(structured outputs), which small local models need to answer in the right
shape; set `structured: false` for a server that rejects it. Replies are
parsed leniently and a reply that still doesn't fit is asked for once more.

Chat backends also take `max_tokens`, `timeout`, and `price_in_per_mtok` / `price_out_per_mtok` to report cost.

## Test cases

A flow's input comes from test cases or from typing it in the TUI; the flow
file itself doesn't hold inputs. Cases live next to the flow:
`examples/inbox.yaml` → `examples/inbox.tests/*.yaml`.

```yaml
description: Boss wants to meet before the board meeting
input:
  sender: boss
  message: Can we sync Thursday about the launch?
expect:                  # optional
  worth_it: yes          # bool: yes / no
  intent: meeting        # choice: an option
  tone.urgency: ">=4"    # score: a condition; node.question inside questions nodes
  sent.action: sent      # a field an action produced (output, llm, http)
```

Flows that fetch data can be tested offline: `http:` gives a canned response
for an http node by name, for any method, instead of calling anything (the
TUI shows it as `FAKED`).

```yaml
input:
  record_id: 7
http:
  fetch:                  # the http node's name
    status: 200           # default
    body: {title: "Logo blurry", status: open}
```

`hunch test examples/inbox.yaml --backend jev` runs every case and reports
pass/fail, which is also how you compare backends: the mock passes 0/3 of the
example cases, Jev 3/3.

## TUI

```sh
go run ./cmd/hunch tui examples/inbox.yaml --backend jev
```

- **Flow** (top left): the flow as a tree from the start node, each edge
  labelled with its branch (`yes ▶`, `sales ▶`, `else ▶`); the path the run took
  is green. A node reachable from several places is drawn once, the others
  point back to it (`↑`). Each node has a coloured badge for its kind
  (`YES/NO`, `CHOICE`, `SCORE`, `MULTI`, `SAY`, `SHELL`) and shows its answer
  in that kind's shape. `●` ran, `·` skipped, `◐` running, `⏸` paused before,
  `✗` failed. After a run, **Outcome** names the node it ended on and what it
  produced.
  `v` switches to the **path** view: just what the run did, as numbered steps
  with each answer and the question as it was asked, the result on top, and
  `⚠ close call` on answers within 0.1 of their threshold.
- **Tests** (bottom left): `✓`/`✗` from the last run of each case. Enter loads a case.
- **Node** (top right): the selected node. The question as actually asked, a
  probability bar per answer, confidence against the threshold, and what the
  previous run answered.
- **Inputs** (bottom right): the current case's input, or unsaved typed input.
  Inputs the flow reads are listed even if empty, and empty ones block a run.
  Enter on a choice or yes/no input opens a picker; on anything else, an
  editor (alt+enter for newlines, e.g. to paste an email; numbers are checked
  against min/max). `w` saves back to
  the case (only its `input:` section changes, expectations stay), `n` saves as
  a new case. Expectations show ✓/✗ under the inputs.
- Files are watched: edit the flow or a case in your editor and the TUI reloads.

Keys: `j/k` move · `tab` next pane · `enter` edit / load case · `r` run (nothing else runs the flow) ·
`s` step one node · `v` graph / path view · `L` live / dry run · `x` stop · `1-4` jump to pane · `b` switch backend · `a` add input · `d` delete input · `w` save case ·
`n` save as new case · `q` quit

## Commands

- `hunch validate FLOW`: dangling routes, impossible branches, uncovered
  answers, unreachable nodes, loops, refs to nodes that haven't run yet.
- `hunch tui FLOW [--backend name] [--set k=v]... [--state f.json] [--runs file.jsonl]`
- `hunch test FLOW [--backend name] [--live] [case...]`: run test cases, check expectations. `http` nodes don't send unless `--live`.
- `hunch tune FLOW [--runs file.jsonl] [--backend name] [--no-tests] [--all]`: see what each threshold would do.
- `hunch serve FLOW [--addr host:port] [--backend name] [--writer name] [--dry-run] [--token-env VAR] [--trace file|dir|s3://...] [--dedupe-key T] [--dedupe-store dir|s3://...]`: run the flow for each webhook POST.
- `hunch run FLOW [--backend name] [--case name] [--set k=v]... [--state f.json] [--trace f.jsonl] [--json] [--max-visits N]`
