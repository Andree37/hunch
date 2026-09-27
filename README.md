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

## Keys

Put API keys in `.env` (gitignored; see `.env.example`). Variables already set in
your environment take precedence.

## Backends

| Kind | Status |
|---|---|
| `mock` | Deterministic answers from a hash of question + state. `answers: {node: value}` or `{node.question: value}` forces answers. |
| `jev` | TypeSafe's Jev via OpenRouter (`provider: openrouter`, needs `$OPENROUTER_API_KEY`) or TypeSafe directly (`provider: typesafe`, `$TYPESAFE_API_KEY`). Also `model`, `api_key_env`, `url`, `timeout`, `max_retries`. Retries 429/529/5xx. |
| `laya` | planned (local, via HTTP sidecar) |
| `ollama` / `llm` | planned |

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
`s` step one node · `v` graph / path view · `x` stop · `1-4` jump to pane · `b` switch backend · `a` add input · `d` delete input · `w` save case ·
`n` save as new case · `q` quit

## Commands

- `hunch validate FLOW`: dangling routes, impossible branches, uncovered
  answers, unreachable nodes, loops, refs to nodes that haven't run yet.
- `hunch tui FLOW [--backend name] [--set k=v]... [--state f.json]`
- `hunch test FLOW [--backend name] [case...]`: run test cases, check expectations.
- `hunch run FLOW [--backend name] [--case name] [--set k=v]... [--state f.json] [--trace f.jsonl] [--json] [--max-visits N]`
