# idem

[![CI](https://github.com/pcanilho/idem/actions/workflows/ci.yml/badge.svg)](https://github.com/pcanilho/idem/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/pcanilho/idem.svg)](https://pkg.go.dev/github.com/pcanilho/idem)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

**Some Helm charts never finish deploying, and your GitOps engine will not tell you which.**

`idem` renders a chart more than once, compares the results, and names the objects that will
never settle under ArgoCD, under Flux, or under plain Helm, with the config that stops it.

```console
$ idem ./examples/churning-chart
  examples/churning-chart/templates/main.yaml
    Secret/churning-chart-secret   .data.password   silent, no checksum

      argocd   CHURNS   on every re-render, at least daily, and without cluster access
      flux     CHURNS   on every chart or values change
      helm     CHURNS   on every `helm upgrade`

      No `lookup` anywhere in this chart, so nothing can stabilise this value.
      That is a chart defect rather than an ArgoCD limitation. Worth reporting
      upstream, and pinning the value meanwhile.

  1 of 1 chart will churn under ArgoCD.
  helm 4.2.4 · 3 rounds

  Add to your ArgoCD Application to stop the churn:

    spec:
      ignoreDifferences:
        - kind: Secret
          name: churning-chart-secret
          jsonPointers: [/data/password]
      syncPolicy:
        syncOptions: [RespectIgnoreDifferences=true]
```

The report has three parts. **What differs**, where `silent, no checksum` means nothing will
restart and nothing will alert. **What your engine does about it**, which comes out as three
different answers because the three engines render under different conditions. And **a block you
can paste**. Flux gets `spec.driftDetection.ignore` instead, on different paths, because the two
engines evaluate them against different shapes.

---

## Why charts churn

This is an extremely common idiom, and it is the one that motivated `idem`:

```gotemplate
password: {{ .Values.auth.password
             | default (lookup "v1" "Secret" .Release.Namespace "creds").data.password
             | default (randAlphaNum 32) }}
```

Under `helm install`, `lookup` finds the existing Secret and the password is stable. Under
ArgoCD, `lookup` returns `{}`, because the repo-server has no cluster access, so the third
branch fires and a new password is generated every time the manifests are rendered again.

What it costs depends on what else references that Secret. A workload with a `checksum/`
annotation rolls its pods on every sync, which is at least visible. A workload without one keeps
running while its Secret drifts away from the password the database still expects, and
**nothing restarts and nothing alerts.**

`argocd app diff` will not catch it either. [From its own docs](https://argo-cd.readthedocs.io/en/stable/user-guide/commands/argocd_app_diff/):

> *Kubernetes Secrets are ignored from this diff.*

The objects where this bug lives are the ones ArgoCD will not show you.

---

## Install

```sh
brew install pcanilho/tap/idem
```

Or grab a binary from the [latest release](https://github.com/pcanilho/idem/releases/latest), or
build it yourself with **Go 1.27** or newer:

```sh
go install github.com/pcanilho/idem@latest
```

`idem` shells out to whatever `helm` is on your `PATH`, the same way ArgoCD's repo-server does.
`idem doctor` and `--context` additionally need `kubectl`. Nothing else.

---

## Quick start

```sh
idem ./charts
```

Point it at a directory and it finds every chart under it. A chart with nothing wrong costs two
lines:

```console
$ idem ./examples/stable-chart
✓ stable-chart renders consistently under ArgoCD.
  helm 4.2.4 · 3 rounds
```

Most runs look like that, which is why findings never fail a run on their own. `--strict` is
what turns them into a non-zero exit, and it is opt-in for exactly that reason.

It also takes a single chart, or one you have not adopted yet. The `./examples/...` charts ship
in this repository, so clone it to run them exactly as shown:

```sh
idem ./charts/my-app           # one chart
idem myapp --repo https://charts.example.com
idem oci://registry.example.com/charts/myapp
```

---

## In CI

### GitHub Actions

A step in your workflow, with `--strict` so a finding fails the build:

```yaml
# .github/workflows/charts.yml
- uses: pcanilho/idem@v0.1.0
  with:
    args: ./charts --strict
```

Findings arrive as **annotations on the run**, and inline in Files Changed wherever `idem` can
place a line, because the action defaults to `-o github`. That needs no token and no extra
permissions. An observed finding names the file but not a line — `idem` will not guess one — so
GitHub shows it inline only when the file's first line is in the diff, and in the Checks tab
otherwise.

### pre-commit

The same check before the commit is made, in `.pre-commit-config.yaml`:

```yaml
repos:
  - repo: https://github.com/pcanilho/idem
    rev: v0.1.0
    hooks:
      - id: idem
```

It fires only when a staged file changes a render: `Chart.yaml`, any `values*.yaml`, or anything
under `templates/`.

### Adopting it on an estate that already has problems

`--new-from-merge-base main` reports only what your branch changed, so the pipeline is green from
day one. [`docs/ci.md`](docs/ci.md) has the rest of both.

---

## Exit codes

| Code | Meaning |
|---|---|
| `0` | Ran fine. Findings are printed but not fatal. |
| `1` | Findings, **and** you passed `--strict`. |
| `2` | A chart could not be rendered, or `idem` itself failed. Always fatal. |

---

## Docs

- [`docs/usage.md`](docs/usage.md): every flag, the output formats, what each engine does, and
  `idem diff`.
- [`docs/ci.md`](docs/ci.md): the GitHub action, pull-request annotations and comments, the
  `--new-from-merge-base` ratchet, and the pre-commit hook.
- [`docs/gitops.md`](docs/gitops.md): what `idem` reads from your Application or HelmRelease, and
  `idem doctor` for churn that has already happened.
- [`docs/limits.md`](docs/limits.md): what it cannot see, and how it compares to other tools.
- [`docs/design.md`](docs/design.md): the reasoning, the evidence, and the places `idem` can
  itself be wrong.
- [CONTRIBUTING.md](CONTRIBUTING.md): how the codebase works. Bug reports are most useful with a
  chart that reproduces the problem; something the size of `examples/churning-chart` beats any
  description.
- [SECURITY.md](SECURITY.md): what `idem` does and does not touch.

## License

Apache 2.0. See [LICENSE](LICENSE).
