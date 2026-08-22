# idem

[![CI](https://github.com/pcanilho/idem/actions/workflows/ci.yml/badge.svg)](https://github.com/pcanilho/idem/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/pcanilho/idem.svg)](https://pkg.go.dev/github.com/pcanilho/idem)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

**Check your Helm charts against the GitOps engine you actually run.**

`idem` renders your chart more than once, compares the results, and tells you which objects will
never settle — under ArgoCD, under Flux, under plain Helm — along with the config that fixes it.

```console
$ idem ./examples/stable-chart
✓ stable-chart renders consistently under ArgoCD.
  helm 4.2.4 · 2 rounds
```

Why that matters: a chart that renders differently every time never converges. The app sits
`OutOfSync`, `selfHeal` re-applies it, and any workload with a `checksum/` annotation rolls its
pods — again and again, for as long as it is deployed.

---

## The failure this exists for

Three Harbor `Deployment`s in my homelab reached **revision 658 over 729 days**. Nothing was
wrong with the cluster. The chart did this, which is an extremely common idiom:

```gotemplate
password: {{ .Values.auth.password
             | default (lookup "v1" "Secret" .Release.Namespace "creds").data.password
             | default (randAlphaNum 32) }}
```

Under `helm install`, `lookup` finds the existing Secret and the password is stable. Under
ArgoCD, `lookup` returns `{}` — the repo-server has no cluster access — so the third branch
fires and a new password is generated. Every time the manifests are rendered again.

The same chart also rewrote a Postgres superuser password until it no longer matched the
database. That `StatefulSet` had no `checksum/` annotation, so **nothing restarted and nothing
alerted.** It was silently broken for two years.

You cannot catch this with `argocd app diff` either —
[from its own docs](https://argo-cd.readthedocs.io/en/stable/user-guide/commands/argocd_app_diff/):

> *Kubernetes Secrets are ignored from this diff.*

The objects where this bug lives are exactly the ones ArgoCD will not show you.

**See it for yourself in ten seconds**, on the most-pulled chart in the ecosystem:

```sh
helm template pg oci://registry-1.docker.io/bitnamicharts/postgresql | grep postgres-password
helm template pg oci://registry-1.docker.io/bitnamicharts/postgresql | grep postgres-password
```

Two different values. Five renders give five passwords.

That is the failure `idem` is built around. It is not the only way a release drifts, and the
others are covered further down in [what makes a release drift](#what-makes-a-release-drift).

---

## Install

Pre-1.0 and unreleased, so build it from source. Needs **Go 1.26** or newer:

```sh
go install github.com/pcanilho/idem@latest
```

`idem` shells out to whatever `helm` is on your `PATH`, the same way ArgoCD's repo-server does.
`idem doctor` and `--context` additionally need `kubectl`. Nothing else.

---

## Try it

Point it at a chart, a directory of charts, or a registry:

```sh
idem ./charts                  # every chart under a directory
idem ./charts/my-app           # one chart
idem myapp --repo https://charts.example.com
idem oci://registry.example.com/charts/myapp
```

Here is a chart that churns, and what `idem` says about it — trimmed to the ArgoCD half, because
the real run prints the matching Flux block too:

```console
$ idem ./examples/churning-chart

  examples/churning-chart/templates/main.yaml
    Secret/churning-chart-secret   .data.password   silent — no checksum

      argocd   CHURNS   on every re-render — at least daily, and without cluster access
      flux     CHURNS   on every chart or values change
      helm     CHURNS   on every `helm upgrade`

      No `lookup` anywhere in this chart, so nothing can stabilise this value.
      That is a chart defect rather than an ArgoCD limitation — worth reporting
      upstream, and pinning the value meanwhile.

  — potential · not counted, not fatal —

    churning-chart
      randAlphaNum   random   examples/churning-chart/templates/main.yaml:7

      This chart already churns, and idem cannot say which function did it —
      a rendered value cannot be traced back to the call that produced it.
      These are where to look first.

  1 of 1 chart will churn under ArgoCD.
  helm 4.2.4 · 2 rounds

  Add to your ArgoCD Application to stop the churn:

    spec:
      ignoreDifferences:
        - kind: Secret
          name: churning-chart-secret
          jsonPointers: [/data/password]
      syncPolicy:
        syncOptions: [RespectIgnoreDifferences=true]
```

Read it in three parts:

- **what differs** — the object, the field, and what it costs you. `silent — no checksum` means
  nothing will restart and nothing will alert; you would never have noticed.
- **what it means for your engine** — the same chart gets three different answers, because the
  three engines render it under different conditions.
- **what to do about it** — a block you can paste. For Flux, `idem` emits
  `spec.driftDetection.ignore` instead; the paths differ, because the two engines evaluate them
  against different shapes.

---

## Three engines, three different answers

This is the part other tools do not do, and it is the reason `idem` exists.

| Engine | Does `lookup` resolve? | So a `lookup`-guarded value is… |
|---|---|---|
| **ArgoCD** | **No** — the repo-server runs `helm template` with no cluster access | **churning** |
| **Flux** | Yes — helm-controller does a real install | stable |
| **Helm** | Yes — `helm upgrade` talks to the cluster | stable |

So "is this chart broken?" has no single answer. A chart using `lookup` is *correct Helm*; it
just cannot work under ArgoCD. `idem` tells you which situation you are in, and whether the fix
belongs in your Application or upstream in the chart.

Without a cluster, `idem` says `unknown` for Flux and Helm rather than guessing. Give it one and
those become measured facts:

```sh
idem ./charts --context=              # your current kube context
idem ./charts --context=prod          # a named one
```

`--context` is opt-in and read-only. It renders through the API server (`--dry-run=server`), so
`lookup` resolves and your real cluster capabilities are used. It never applies anything.

It also reports **what the cluster would rewrite as it admits each object** — under
`the cluster rewrites these on admission`, marking each field as one the cluster *defaults* or
one it *assigns*. Most of that is ordinary API-server defaulting your engine normalises away.
A mutating webhook writing into a field your chart also sets is not, and that is a drift loop
you cannot see from `helm template` alone.

---

## What makes a release drift

Something ends up in your cluster that git does not describe. That happens at one of five
stages, and each needs different evidence — so no single check finds them all:

| Stage | What happens | Does `idem` check it? |
|---|---|---|
| **Source** | a `Chart.yaml` version range resolves to a new subchart, with no git change | **No.** Analysed in [`docs/design.md`](docs/design.md), not built |
| **Render** | the chart renders differently every time | **Yes** — `idem ./charts`, offline |
| **Engine** | ArgoCD or Flux adds, strips or rewrites fields after rendering | **Yes** — every pointer is checked against what that engine actually evaluates |
| **Admission** | the API server or a webhook mutates the object as it is applied | **Yes** — `idem ./charts --context=` |
| **Post-apply** | another controller writes into the object *seconds later* | **Yes** — `idem doctor --namespace <ns>` |

Two things worth being straight about.

**Render-side churn is a minority of real `OutOfSync` reports.** Admission and defaulting cause
far more of them. What makes the render-side case worth a tool of its own is that it is the only
one on this list you can catch **offline and before merge** — the others need a running cluster,
and the last needs the drift to have already happened. `idem` checks those too when you give it
a cluster; it just cannot check them in a pull request.

**Source-side is not built.** The analysis is written up in the design notes and the check would
be a single label comparison, but nothing implements it, so the table says no.

---

## In CI

Findings are informative by default. `--strict` turns them into a failing build:

```yaml
- uses: pcanilho/idem@v1
  with:
    args: ./charts --strict
```

`-o github` emits workflow commands, so findings appear **inline on the diff** in Files Changed —
no token, no API calls, no extra permissions. To post one summary comment instead:

```yaml
- run: idem ./charts -o markdown > /tmp/idem.md
- run: gh pr comment ${{ github.event.number }} --body-file /tmp/idem.md
  if: ${{ hashFiles('/tmp/idem.md') != '' }}
```

A clean run writes **nothing**, so the guard is what stops a comment saying everything is fine on
every pull request that touches a chart.

Adopting `idem` on an estate that already has problems? Report only what your branch changed:

```sh
idem ./charts --new-from-merge-base main
```

A permanently red pipeline gets switched off, so the ratchet exists to keep it green from day
one. It filters *findings* only — a chart that will not render at all is still reported, because
that is a gap in what was checked rather than a finding about it.

### Before the commit, not after

`idem` ships a hook for [pre-commit](https://pre-commit.com) and
[prek](https://github.com/j178/prek):

```yaml
repos:
  - repo: https://github.com/pcanilho/idem
    rev: v0.1.0
    hooks:
      - id: idem
```

It runs only when something that changes a render is staged — `Chart.yaml`, any `values*.yaml`,
anything under `templates/` — and checks **every chart in the repository**, because a `values.yaml`
edit changes what every template in that chart renders.

You need Go (the hook builds `idem` itself) and `helm` on your `PATH`. To report without blocking
the commit, override the default `--strict`:

```yaml
      - id: idem
        args: []
```

---

## Already fixed it? `idem` knows

`idem` reads the ArgoCD `Application` / `ApplicationSet` and Flux `HelmRelease` in your
repository. That tells it three things:

- **what you already suppress** — a finding covered by your own `ignoreDifferences` is shown as
  handled, not shouted about again, and it does not fail `--strict`.
- **what your chart is rendered with** — values, release name, and namespace come from the
  manifest that deploys it, because a chart rendered with no values is a release nobody runs.
  ApplicationSet generators that read the repository are expanded, one release per element.
- **which engines you use** — so you only see verdicts and fix blocks for engines you run.

One case is worth calling out: an `ignoreDifferences` block with `selfHeal: true` and no
`RespectIgnoreDifferences=true` hides the diff while re-applying the object anyway. `idem`
reports that as a trap rather than as handled, because you believe it is fixed and it is not.

---

## Find it in a cluster you already run

Everything above predicts churn. `idem doctor` finds churn that has already happened — no chart
needed:

```sh
idem doctor                      # what keeps rolling, and who owns it
idem doctor --namespace lab      # what is being written after apply
```

It ranks workloads by how often they roll against the cluster's own median, names the Application
or HelmRelease that owns each, and resolves that to a chart path — so the last line is a command
you can run. It calls that triage rather than proof, because deploying often looks the same from
here.

`--namespace` answers the other question: which fields were **written after the apply**, and by
what. It separates *applied absent, live set* from *applied and live differ*, and names the
controller when the object carries evidence of one — cert-manager, External Secrets, an operator.
Where it cannot tell, it says so instead of guessing a name you would then go and chase.

---

## Compare two renders yourself

```sh
idem diff a.yaml b.yaml
```

The comparison engine on its own: no helm, no network, no cluster. This is also how you point
`idem` at kustomize:

```sh
kustomize build overlays/prod > a.yaml
kustomize build overlays/prod > b.yaml
idem diff a.yaml b.yaml
```

---

## Commands and flags

```
idem [chart] [flags]     check a chart, or every chart under a directory
idem diff a.yaml b.yaml  compare two renders you produced yourself
idem doctor [flags]      ask a cluster you already run what keeps rolling
```

| Flag | What it does |
|---|---|
| `-f`, `--values` | values file, repeatable |
| `--set` | set a value, repeatable |
| `--rounds` | how many renders to compare (default 2) |
| `--strict` | exit 1 when something will churn |
| `-v` | expand every finding instead of capping each at five fields |
| `-o` | `text`, `json`, `markdown` or `github` |
| `--engine` | `argocd`, `flux`, `helm`, `all`, or `auto` (default) |
| `--context` | resolve `lookup` and capabilities against a cluster |
| `--namespace` | render into this namespace instead of the one your config names |
| `--repo` | chart repository URL, as helm's `--repo` |
| `--chart-version` | chart version to fetch, as helm's `--version` |
| `--jobs` | renders to run at once |
| `--new-from-rev`, `--new-from-merge-base` | report only what changed |
| `--dependency-update`, `--no-deps` | how to handle missing subcharts |
| `--helm` | which helm binary to render with |
| `--version` | print idem's version |

Flags may come before or after the chart path.

`-o json` is the machine-readable contract, so you can gate on it however you like:

```sh
idem ./charts -o json | jq '.findings[] | select(.consequence == "rolls")'
idem ./charts -o json | conftest test -
```

---

## Exit codes

| Code | Meaning |
|---|---|
| `0` | Ran fine. Findings are printed but not fatal. |
| `1` | Findings, **and** you passed `--strict`. |
| `2` | A chart could not be rendered, or `idem` itself failed. Always fatal. |

---

## What it does not do

- It does not talk to your cluster unless you pass `--context`, and even then only to render —
  never to apply, create, update, or own anything.
- It never writes to your repository. Subcharts resolve in a temp directory unless you ask
  otherwise with `--dependency-update`.
- It does not reconstruct your delivery pipeline — no kustomize overlays, no `postBuild`
  substitution, no post-renderers. It renders what you point it at and names what it could not
  see.
- It does not prove a chart is free of secrets. It proves the output is *stable*, which is the
  property ArgoCD actually needs.
- It renders twice **back to back**, so it cannot see non-determinism that unfolds over *time*:
  an unpinned dependency range, a floating `:latest` tag, a re-published chart version. A clean
  run means "this renders consistently right now, with this helm" — not "this chart is pinned
  forever".

---

## How it compares

| Tool | Compares | Finds a chart that churns? | Writes the fix? |
|---|---|---|---|
| **`idem`** | one release against **itself**, twice | **yes** | **yes** |
| `argocd app diff` | desired vs live, once — *and ignores Secrets* | no | no |
| `helm diff` | two different chart versions | no | no |
| `helm unittest` | this render vs a committed snapshot | goes red, but re-baselining hides it | no |
| kubeconform, kube-score, polaris | schema and policy on one render | no | no |
| conftest / OPA | policy on one render | no | no |

Everything else compares two *different* things once. Non-determinism only shows when you compare
a release to itself — and telling it apart from genuine drift is what makes the fix different in
each case.

---

## Design notes

The reasoning, the evidence, and the places `idem` can itself be wrong are in
[`docs/design.md`](docs/design.md) — including why there is no rules file, why input type is
detected rather than declared, and what each engine actually does, checked against its source.

## Contributing

Bug reports are most useful with a chart that reproduces the problem — something the size of
`examples/churning-chart` is worth more than any description. See
[CONTRIBUTING.md](CONTRIBUTING.md) for how the codebase works, and
[SECURITY.md](SECURITY.md) for what `idem` does and does not touch.

## License

Apache 2.0. See [LICENSE](LICENSE).
