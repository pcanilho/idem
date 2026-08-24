# Contributing

Thanks for looking. `idem` is small on purpose — one dependency, no config file, no plugins — so
the bar for adding surface is high, and the bar for adding *evidence* is low.

## Getting set up

```sh
git clone https://github.com/pcanilho/idem && cd idem
go build ./...
go test ./...
```

You need **Go 1.27** and **helm** on your `PATH`. Tests that need helm skip themselves when it is
absent, so a green run without it proves less than you might think — install helm before
trusting a local pass. Tests that need a cluster skip themselves too.

Try your build on the bundled examples:

```sh
go run . ./examples/churning-chart
go run . ./examples/stable-chart
```

## Before you open a pull request

CI runs on ubuntu and macos and checks all of these, so run them first:

```sh
gofmt -l .          # must print nothing
go vet ./...
go fix ./...        # must reach a fixed point — commit anything it changes
go test ./... -race
```

## How this codebase works

Three conventions do most of the work here. They are unusual enough to be worth stating.

### Verify, don't recall

Every claim `idem` makes about ArgoCD, Flux, Helm or sprig was checked against source or docs,
and the citation is recorded in `docs/design.md`. This matters more than it sounds: on first
writing, **four** of those claims were wrong, and one shipped — `idem` told users a chart churned
"every sync, forever" when ArgoCD actually caches rendered manifests for 24h.

If you are adding a claim about someone else's tool, read their code. A plausible memory is not
evidence, and this tool's entire value is that its output can be trusted.

### Test first, then try to break the test

Write the test, watch it fail **on the assertion** — a compile error is not a red — then
implement. Then break the implementation on purpose and confirm the test catches it.

That last step is not ceremony. Tests in this repository have been caught passing vacuously
several times, including one that asserted on a phrase which later changed, and silently stopped
checking anything at all. Mutate production code, never the test.

### Comments explain why, not what

Comment density here is high on purpose, and the useful ones say why the obvious alternative was
rejected. `// increment i` helps nobody; `// Written back verbatim rather than emptied, which is
what argo-cd does — and it is what lets idem notice` is the comment that stops someone
"simplifying" a load-bearing decision next year.

## Things that are deliberately not here

Please read [`docs/design.md`](docs/design.md) before proposing one of these — each was argued
through, and reopening one needs new evidence rather than a fresh preference:

- a rules file or an exceptions file (`-o json` is the seam; OPA is the policy engine)
- a `--type` flag (if the user has to say what it is, the tool should have looked)
- HTML, CSV or SARIF output
- vendoring the Helm SDK instead of shelling out to `helm`

## Reporting something wrong

The most useful bug report for this tool is a **chart that reproduces it**. If you can reduce it
to something the size of `examples/churning-chart`, that is worth more than any description.

Include `idem --version`, `helm version --short`, and which engine you run.
