# Contributing

## Setup

```bash
git clone https://github.com/jagreehal/errpath-go.git
cd errpath-go
make test
```

## Checks

```bash
make quality   # format, lint, test
make ci        # quality + build
```

Prefer table-driven tests. Keep coverage high for the analyser and MachineJSON emitter.

## Pull requests

- Keep changes focused
- Match Go style used in the repo (`gofmt`, `goimports`)
