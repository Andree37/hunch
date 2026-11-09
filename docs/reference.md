# hunch reference

Decision flows with typed, calibrated answers.

A flow is a YAML graph. **Decision nodes** ask a model a typed question
(`bool`, `choice`, `score`, or several at once) and branch on the answer and
its confidence. **Action nodes** do things. Every result lands in shared state,
so later nodes build on earlier ones via `{{node.field}}` refs.

Models are swappable: Jev, Laya, chat models, or the built-in deterministic
mock, listed once per project in `hunch.yaml` (see [Models](#models)).

```sh
go run ./cmd/hunch tui examples/inbox.yaml
go run ./cmd/hunch run examples/inbox.yaml --set sender=boss --set message="Can we sync Thursday?"
```

## Flow file

```yaml
name: example
threshold: 0.6          # below this confidence, `unsure` routes win (default 0.6)
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
| `llm` | `prompt`, `system`, `max_tokens`, `model` | `text` |
| `http` | `url`, `method` (default POST), `headers`, `body`, `timeout` (s, default 30), `retries` (default 2), `idempotency_key` | `status`, `body` (parsed if JSON) |
| `output` | `set: {name: value}` | the values; also the flow's **outputs** |

```yaml
nodes:
  draft:                # written by hunch.yaml's writer (or model: on the node)
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
    headers: {Authorization: "Bearer $API_TOKEN"}   # $VARS come from the environment
    body: {reply: "{{draft.text}}"}
    then: sent
  sent: {action: output, set: {action: sent}}
  hold: {action: output, set: {action: needs_human, reply: "{{draft.text}}"}}
```

- **Sending is a decision.** Put the "should this go out?" question before
  the `http` node, as above.
- **Dry runs.** `hunch test` and the TUI never write: `shell` nodes record
  the command they would run (with empty `stdout` and `exit_code` 0), and
  `http` nodes that POST/PUT/PATCH/DELETE record the request they would make.
  GETs still run, since they only read and later nodes need the data. Use
  `hunch test --live` or `L` in the TUI to do it for real. `hunch run` does
  unless given `--dry-run`.
- **Secrets** go in `$VARS` in `url` and `headers`, read from the environment,
  so they never enter state or any model's prompt. Only `$VARS` written in
  the `url` or header itself are expanded: a `$NAME` that comes in through
  a ref (an input, an answer, or a `state:` constant) stays as it is. Write
  `url: "$API_BASE/hook"`, not `url: "{{api}}/hook"` with `api: $API_BASE`
  in `state:`. Refs in a URL are escaped.
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

## Recording and replaying runs

`hunch run` and the TUI record every run: its input, each step
(question as asked, what the model saw, probabilities, route, cost), and how
it ended. By default runs go next to the flow, like its tests:
`examples/triage.yaml` → `examples/triage.runs/`. Set `runs:` in the flow to
record elsewhere (a path, relative to the flow file),
`runs: off` to not record, or `--trace` to override it for one command.
A location can be:

| `--trace` | Stored as |
|---|---|
| `runs.jsonl` | one file, appended to, rolling over at `--trace-max-mb` (100) and keeping 3 old files |
| `./runs/` | one file per run, named by start time: `2026-09-27/18-17-19.123-<id>.jsonl` |

The TUI reads the flow's runs by itself:

```sh
hunch run examples/hello.yaml --set message="Can we sync Thursday?"   # records to examples/hello.runs/
hunch tui examples/hello.yaml                                         # lists and replays them
```

- Pane 2 lists the flow's recorded runs, newest first (`t` switches to test
  cases). Only runs of the open flow are listed; runs of other flows in the
  same place are hidden, with the command to open them. `o` reads runs from
  somewhere else (a folder or a `.jsonl` file), as does `--runs`; it
  updates while runs keep being recorded (a file on change, a directory
  every 10s, in the background).
- Enter loads a run into the Run and Map views, with its input in the Input pane.
- `esc` (or Enter on it again) closes it and puts back what you had before.
- `s` replays it one step at a time, exactly as it happened; `r` runs the same
  input again live, e.g. after changing the flow, to see if it now goes the
  way it should.
- `c` marks a run; `c` on another compares the two side by side: the input
  that differs, where their paths split, and each question's answer and
  confidence in both, with the ones that differ marked. `esc` goes back.
- `n` saves it as a test case: its input, the http replies it got (as fakes,
  so it runs offline) and every answer it gave as `expect:`. Fix the answers
  it got wrong and the test fails until the flow gets them right. `n` does the
  same after any finished run in the TUI; with no run of the current inputs on
  screen it saves just the inputs.

## The triage example

`examples/triage.yaml` is triage for a network team. Given a ticket id, it
fetches the ticket from `TICKETS_API` and:

1. **Whose ticket is it?** Jev picks the team from what each team handles
   (network, desktop, identity, applications). "My computer is down" is ours
   only if the problem is its connection. Not ours: the ticket is moved to that
   team's queue with a note saying why.
2. **Ours: is the severity right?** A rule checks only sev2 and sev2.5; Jev
   picks the severity under the SLA, only the same or lower.
3. Jev establishes the facts the SLA turns on in one multi-question call: no
   connection at all? workaround? how many people? what kind of ticket?
4. The writer model (Ollama on this machine, say) writes the note from the SLA,
   the decision and those facts; Jev checks it, and a plain note built from
   the facts is posted if it's off.

The 12 test cases in `examples/triage.tests/` cover ownership and severity,
including both readings of "my computer is down":
`go run ./cmd/hunch test examples/triage.yaml` (12/12 with Jev deciding).
They fake the ticket fetch and never write.

## Examples

Each is a different shape of the same building blocks; none is special.

| Flow | Shows |
|---|---|
| `examples/inbox.yaml` | Chained decisions: gate, classify, multi-question, score, route on confidence. |
| `examples/respond.yaml` | Decide, write with a model, judge the result, then send it or hand it to a person. |
| `examples/triage.yaml` | Ticket triage: a model decides ownership and reroutes, a rule limits scope, a model re-classifies (only downwards), a second model writes a note that the first one checks, the source record is updated. |

Each has a `<flow>.tests/` folder you can run with `hunch test`.

## Models

Flows don't say which models they use. `hunch.yaml`, in the flow's folder or
any folder above it, lists them once for the whole project:

```yaml
default: jev       # decides, unless --model or b in the TUI says otherwise
writer: ollama     # writes text for llm nodes (optional)

models:
  jev:    {url: https://api.typesafe.ai/v1/systemone, key: tsk-...}
  laya:   http://localhost:8000/v1/systemone
  claude: {url: https://api.anthropic.com/v1/messages, key: sk-ant-..., model: claude-opus-5-5}
  ollama: {url: http://localhost:11434/v1/chat/completions, model: "qwen2.5:1.5b"}
  gpt:    {url: https://my-team.openai.azure.com/openai/v1/chat/completions, key: ..., model: gpt-5}
  nova:   {url: https://bedrock-runtime.us-east-1.amazonaws.com, model: amazon.nova-pro-v1:0}
```

A model is the URL of its endpoint. A plain string is just the URL; a map
adds `key` (sent as the server expects it) and `model` (when the server runs
several). The path says which API it speaks:

| Endpoint | Speaks | Decides | Writes |
|---|---|---|---|
| `.../v1/systemone`, `.../decisions` | Jev's decision API: Jev (TypeSafe, OpenRouter's `.../api/alpha/decisions`, Runware), Laya | ✓ calibrated | |
| `.../chat/completions` | OpenAI-style chat: OpenAI, Ollama, OpenRouter, Groq, vLLM, LM Studio, Azure OpenAI | ✓ self-reported | ✓ |
| `.../v1/messages` | Anthropic | ✓ self-reported | ✓ |
| `https://bedrock-runtime.REGION.amazonaws.com` | Amazon Bedrock (your AWS login, no key; `profile` picks one) | ✓ self-reported | ✓ |

A URL whose path says none of these is refused with the endpoints to give
instead. `mock` is always there (deterministic, offline, made-up answers);
`mock: {answers: {node: value}}` forces answers, and any name with
`url: mock` is a mock too.

- **Keys**: `hunch.yaml` holds them, so keep it out of git. `key: $NAME`
  reads one from the environment instead, for files you do commit.
- **Picking**: `--model` picks who decides and `--writer` who writes, on
  `run`, `test` and `tui`; in the TUI, `b` lists the models (`enter` decides
  with one, `w` writes with one). Opening the list asks each model one small
  yes/no question and shows which answer (`✓ 299ms`) and why the others
  don't (`✗ the key is wrong or missing`, `✗ can't reach it: is it
  running?`); `c` asks again. A node can say `model: claude` to always
  use one.
- **Other options** pass through: `timeout` (seconds; 30 for decision
  models, 120 for chat, 0 for no limit), `max_tokens`, `price_in_per_mtok` /
  `price_out_per_mtok` (and `price_per_mtok` for decision models) to report
  cost, and `structured: false` for an OpenAI-style server that rejects JSON
  schemas.
- With no `hunch.yaml`, every command says how to start one; `--model mock`
  still runs a flow offline.

```sh
hunch test examples/respond.yaml --model jev                     # 3/3
hunch test examples/respond.yaml --model ollama --writer ollama  # a 1.5B local model: 1/3
```

Routing on confidence (`unsure`, `threshold`) is most meaningful with
decision models. The validator warns when a chat model's self-reported
confidence drives a route, and the TUI marks those numbers `self-reported`;
check such thresholds against your test cases.

For decisions, OpenAI-style models are sent a JSON schema the server must
follow (structured outputs), which small local models need to answer in the
right shape. Replies are parsed leniently and a reply that still doesn't fit
is asked for once more. Anthropic models use only the key and URL in
`hunch.yaml`; `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN` and SDK profiles
in the environment are ignored.

### Laya on your machine

Laya is a small open decision model (about 421M parameters) that speaks
Jev's API:

```sh
python -m pip install "laya[serve]"   # Python 3.10+
laya-serve                            # http://localhost:8000; LAYA_MODELS=english loads only the English model
```

and in `hunch.yaml`, `laya: http://localhost:8000/v1/systemone`. Run on a
server (an Azure Container App, say), it's the same with that URL and the
`LAYA_API_KEY` the server was started with as `key`. hunch reads Laya's
confidence on Jev's scale (`x_jev_confidence`). It is much smaller than Jev
and can be overconfident, so check your thresholds against the tests (`R`,
then `[` `]`) before trusting it.

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

`hunch test examples/inbox.yaml --model jev` runs every case and reports
pass/fail, which is also how you compare models: the mock passes 0/3 of the
example cases, Jev 3/3.

## TUI

```sh
go run ./cmd/hunch tui                       # pick a flow from this folder
go run ./cmd/hunch tui examples/triage.yaml  # or open one directly
```

With no flow given, the TUI starts on a picker listing every flow under the
current folder, with its description and how many steps, tests and recorded
runs it has. `F` inside a flow goes back to it.

The screen reads left to right: what goes in, what happened, and why.

- **Header**: the flow, where the input came from (a test, a recorded run, or
  typed), the model, and dry run or LIVE. On the right, how the run went:
  how many expectations held, steps, cost and time, or what is running now.
- **2 Tests** (top left): `✓`/`✗` from the last run of each case. `r` runs
  the highlighted one (`s` steps through it); `enter` only loads it. `t` switches to the flow's **recorded runs**, two lines each: when
  and where it ended, then its input.
- **4 Input** (bottom left): the current input. Inputs the flow reads are
  listed even if empty, and empty ones block a run. `enter` on a choice or
  yes/no opens a picker; on anything else, an editor that takes the whole
  screen (alt+enter for newlines, e.g. to paste an email; numbers are checked
  against min/max). `w` saves back to the case (only its `input:` section
  changes), `n` saves it as a new case.
- **1 Run** (middle): the run as a story. The input on top, then each step:
  where it went (`→ intent`), the question as it was asked, and the answer
  with a bar of how sure the model was and a tick at the threshold. Answers
  under the threshold say `not sure enough` (went to `unsure`) or
  `under threshold` (no unsure route); within 0.1 of it, `close call`. A test's
  expectations show as `✓` or `✗ expected …` on their step. It ends with where
  the run ended, and any expectation about a step that never ran.
- **1 Map** (`m`): the whole flow as a tree from the start node, each branch
  labelled (`yes →`, `else →`). The branches the run took are green, so a node
  reachable from several places (drawn once, the others say `↑ drawn above`)
  shows which way it was reached. Before any run, the middle pane is the map.
- **3 Step** (right): the selected step. The question as asked, a bar per
  answer, a sentence on why that answer led where it did (sure enough, not
  sure enough, or under the threshold with no unsure route), the routes with
  the one taken marked, what the test expected, and what the previous run
  answered.
- **All tests** (`R`): every test case at once, 4 at a time, as a grid: one
  row per test, one column per question (and rule) any test reached, then
  where it ended. Each cell is the answer, yellow for a close call or under
  the threshold, orange when it went to `unsure`, with `✓`/`✗` when the test
  expects something of it. Above: how many passed, close calls, cost and
  time. Below: every failed expectation in words. `enter` opens a test's run
  without running it again; `g` comes back to the grid. Grid runs are dry and
  aren't recorded.
- Files are watched: edit the flow or a case in your editor and the TUI
  reloads. With the grid up, every save runs all the tests again and marks
  what changed: `✗→✓` or `✓→✗` on a test, `*` on an answer that changed, and
  `was 9/12` in the summary.
- **Trying thresholds**: `←`/`→` pick a question (the selected one is
  underlined; a row under the names shows each threshold), and `[` / `]`
  move its threshold down or up by 0.05. The grid routes each test again on
  the answers the model already gave, so it costs nothing and is instant:
  `✓→✗` marks what the new threshold breaks, against the grid as the model
  ran it, and a tried threshold shows as `0.85*`. A test that now reaches a
  question the model was never asked shows `?`; `A` asks the model only
  what those tests need, and keeps the answers. Runs with `r` use the tried
  thresholds too. `w` writes them into the flow as each node's
  `threshold:`, changing only those lines, and the tests run again.

The footer lists the keys that matter where you are; `?` lists them all.
`r` run · `R` run every test · `g` back to all tests · `←`/`→` pick a question · `[`/`]` try its threshold · `w` keep thresholds · `A` ask what `?` tests need · `s` step one node · `x` stop · `m` run / map · `1-4` jump to pane ·
`tab` next pane · `j/k` move · `enter` load / open / edit · `b` pick a model ·
`L` live / dry run · `a`/`d` add / delete input · `w` save case · `n` save as
new case · `t` tests / recorded runs · `c` compare two runs · `o` runs from elsewhere · `esc` close a
recorded run · `F` flow picker · `q` quit

## Commands

Every command checks the flow when it loads it (dangling routes, impossible
branches, uncovered answers, unreachable nodes, loops, refs to nodes that
haven't run yet) and stops on errors.

- `hunch tui [FLOW] [--model name] [--writer name] [--set k=v]... [--state f.json] [--runs place]`: without FLOW, pick one
- `hunch test FLOW [--model name] [--live] [case...]`: run test cases, check expectations. `shell` and `http` write nodes only record unless `--live`.
- `hunch run FLOW [--model name] [--case name] [--set k=v]... [--state f.json] [--trace f.jsonl] [--json] [--max-visits N]`
