# hunch

Decision flows in YAML. Ask a model typed questions (yes/no, pick one, score
1–5), branch on the answer **and how sure it is**, then act: call an API, run a
command, write a reply.

![hunch tui](docs/demo/tui.gif)

## Install

```sh
go install github.com/Andree37/hunch/cmd/hunch@latest
```

The examples run offline with the built-in `mock` backend. For real answers,
put `TYPESAFE_API_KEY=...` in `.env` and add `--backend jev`.

## A flow

[`examples/hello.yaml`](examples/hello.yaml):

```yaml
name: hello

backends:
  default: mock                         # offline, deterministic
  mock: {kind: mock, seed: demo}
  jev: {kind: jev, provider: typesafe}  # --backend jev, needs TYPESAFE_API_KEY

nodes:
  worth_it:
    bool: "Does this message need a reply? {{message}}"
    then: {yes: intent, no: archive, unsure: ask_me}   # unsure = low confidence

  intent:
    choice: "What do they want?"
    options: [meeting, info, sales]
    then: {sales: archive, _: reply}

  reply:   {action: log, message: "Reply to: {{message}}"}
  archive: {action: log, message: Archived.}
  ask_me:  {action: log, message: "Not sure, you decide."}
```

## Run it

```sh
hunch run examples/hello.yaml --backend jev --set message="Can we sync Thursday?"
```

![hunch run](docs/demo/run.gif)

## Test it

Test cases sit next to the flow (`hello.yaml` → `hello.tests/`) and say what
each answer should be. Run them to compare models or catch regressions.

```yaml
# examples/hello.tests/meeting.yaml
input:
  message: Can we sync Thursday about the launch?
expect:
  worth_it: yes
  intent: meeting
```

```sh
hunch test examples/hello.yaml --backend jev
```

![hunch test](docs/demo/test.gif)

## Explore it

```sh
hunch tui examples/hello.yaml --backend jev
```

Load a test case, press `r`, and see the path it took, each question as asked,
and the probability behind each answer (the GIF at the top).

## Hook it up

```sh
hunch serve examples/hello.yaml   # runs the flow on every POST
```

Every run is recorded next to the flow, and can be replayed in the TUI or
saved as a test case.

## Examples

| Flow | What it does |
|---|---|
| [`hello.yaml`](examples/hello.yaml) | The flow above. |
| [`inbox.yaml`](examples/inbox.yaml) | Triage an incoming message, with scores and several questions at once. |
| [`respond.yaml`](examples/respond.yaml) | Draft a reply, check it, send it or hand it to a person. |
| [`triage.yaml`](examples/triage.yaml) | Route and re-grade support tickets from a webhook ([demo](examples/ticketdesk)). |

## More

Everything else (node types, actions, backends, serving, recording, tuning
thresholds) is in **[docs/reference.md](docs/reference.md)**.

GIFs are recorded with [VHS](https://github.com/charmbracelet/vhs):
`docs/demo/record.sh`.
