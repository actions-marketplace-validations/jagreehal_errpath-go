---
name: errpath
description: >
  Runs errpath on Go code and fixes what it reports: new panics (no-panic),
  dropped errors (no-swallow), handled errors that never reach OpenTelemetry
  (no-trace) and allow comments without a reason (bad-allow). Use when changing
  Go error handling, before opening a pull request, when an errpath review
  comment or annotation appears, or to draw a function's error paths.
---

# errpath for Go

errpath reads Go source and reports how a change affects error paths. It needs no build and no config.

```bash
go install github.com/jagreehal/errpath-go/cmd/errpath@latest
```

## Check your change

Run this after you edit Go code, from the repository root:

```bash
errpath review --base main --format json -o /tmp/errpath.json
jq '.risk, .newFindings' /tmp/errpath.json
```

Each entry in `newFindings` has `rule`, `file`, `line`, `message`, `severity` and `confidence`. Fix every new finding your change introduced, then run the review again. Drop `--format json` to read the Markdown report a reviewer sees.

`risk` is `high` for a panic that will certainly happen when reached. A dropped error, a handler that records nothing, a possible panic or a changed error path gives `moderate`.

## Fix each rule

**no-swallow**: `_ = f()`, `v, _ := f()` or a bare `f()` drops an error. Return it, wrapped with `%w`:

```go
if err := SendConfirmation(receipt); err != nil {
    return Receipt{}, fmt.Errorf("send confirmation: %w", err)
}
```

**no-panic**: `panic`, `log.Fatal`, `os.Exit` or a `Must*` call on a path that can fail. Return an error instead. Keep `Must*` for constants at startup, with an allow comment.

**no-trace**: an `if err != nil` handler that keeps going without recording the error. Record it on every path through the handler, with the request's context:

```go
if err := SendConfirmation(receipt); err != nil {
    slog.WarnContext(ctx, "confirmation email failed", "error", err)
    retryLater(receipt)
}
```

These count as recording: any `log/slog` call, `span.RecordError(err)`, `span.SetStatus(codes.Error, ...)`, `span.AddEvent(...)`, any autotel call, or a module function that records on every path. Returning or storing the error counts too. `log.Printf`, `fmt.Println` and logging inside a `go` statement do not.

An early `return` or `continue` before the log call leaves that path unrecorded. Move the log above it.

**bad-allow**: an allow comment without a reason. Add the reason.

## Allow a deliberate case

Put the comment on the line above the statement, with a real reason:

```go
// errpath: allow(no-swallow) metrics are best effort
_ = SendMetrics()
```

Rules: `no-swallow`, `no-panic`, `no-trace`. Add an allow comment only when the behaviour is intended. Tell the user about each one you add.

## Name a function's errors

When a function returns errors from several places, errpath labels it `error`. Name them in the doc comment:

```go
// errpath: error PriceError
func Price(cart Cart) (int, error) { ... }
```

## Draw a function

```bash
errpath diagram ./path/to/pkg PlaceOrder    # Mermaid flowchart
errpath machine ./path/to/pkg PlaceOrder    # XState machine (JSON)
```

`PlaceOrder` can also be `Service.Put` or a full id such as `checkout.PlaceOrder`. Paste the Mermaid into a PR or issue and GitHub draws it.

## Pull request reviews

```yaml
permissions:
  contents: read
  pull-requests: write
jobs:
  review:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - uses: jagreehal/errpath-go@v0.1.1
        with:
          fail-on: high # never, moderate or high
```

The Action keeps one comment per PR, annotates new findings inline and writes the report to the job summary.

## Limits

errpath reads source without type checking. It skips methods on types from other modules (`f.Close()` on an `*os.File`), so pair it with errcheck for those. A method on a value it can't type gives a `possible` finding rather than `certain`.
