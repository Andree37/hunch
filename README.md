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
scope:
  switch: "{{ticket.severity}}"
  then: {sev2: check_sev2, sev2.5: check_sev2_5, _: skip}
```

### Shared definitions and outputs

The flow's `state:` holds constants every node can use, so a definition is
written once. They're sent to deciders along with the input, and are never
asked for as inputs. Refs work in answer descriptions and score levels too,
and keys may contain dots (`{{sla.sev2.5}}`).

```yaml
state:
  sla:
    sev2: A core feature is down for many users, no workaround.
    sev2.5: Degraded for some users, or a workaround exists.
nodes:
  check:
    choice: "Which severity fits? {{ticket.title}}"
    options: {sev2: "{{sla.sev2}}", sev2.5: "{{sla.sev2.5}}"}
```

Once an `output` node has run, later nodes can read everything set so far as
`{{outputs.name}}`, whichever branch set it.

### Actions

Decisions steer; actions do the work. Every action's result lands in state
under its node name, so later nodes can use it.

| Action | Fields | Result |
|---|---|---|
| `log` (shown as SAY) | `message` | `message` |
| `shell` | `run`, `timeout` (s, default 60) | `stdout`, `exit_code` |
| `llm` | `prompt`, `system`, `max_tokens`, `backend` | `text` |
| `http` | `url`, `method` (default POST), `headers`, `body`, `timeout` (s, default 30) | `status`, `body` (parsed if JSON) |
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
input, the reply is the path taken and the outputs. Point a webhook at it
(e.g. "ticket created").

```sh
hunch serve examples/severity.yaml --backend jev --token-env HOOK_TOKEN
curl -H "Authorization: Bearer $HOOK_TOKEN" -d '{"ticket": {...}}' http://127.0.0.1:8080/
# {"path": ["scope", "from_sev2", ...], "outputs": {"action": "lowered", "to": "sev3"}, "cost_usd": 0.00003}
```

- Listens on `127.0.0.1:8080` unless `--addr` says otherwise.
- `--token-env VAR` requires `Authorization: Bearer <$VAR>`.
- Inputs are checked like everywhere else; bad or missing ones get a 400.
- Live by default; `--dry-run` records writes instead of sending them.
- A failed run returns 500 with the error and `failed_at` node.
- `--trace file.jsonl` keeps every run's events; `GET /healthz` for checks.

See `examples/severity.yaml`: only sev2 / sev2.5 tickets are checked (a rule),
the model picks a severity that can only stay or go down, a writer explains
why, and the ticket is updated and commented on.

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
- `hunch tui FLOW [--backend name] [--set k=v]... [--state f.json]`
- `hunch test FLOW [--backend name] [--live] [case...]`: run test cases, check expectations. `http` nodes don't send unless `--live`.
- `hunch serve FLOW [--addr host:port] [--backend name] [--dry-run] [--token-env VAR] [--trace file]`: run the flow for each webhook POST.
- `hunch run FLOW [--backend name] [--case name] [--set k=v]... [--state f.json] [--trace f.jsonl] [--json] [--max-visits N]`
