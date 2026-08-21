# idem

**Check your Helm charts against the GitOps engine you actually run.**

`idem` is a command-line tool. It renders your chart more than once, compares the results
structurally, and tells you which objects will never settle — under ArgoCD, under Flux, under
plain Helm — along with the exact config to fix it.

```console
$ idem ./charts
✓ 10 charts render consistently   ·   helm 4.2.4, 2 rounds
```

Why that matters: a chart that renders differently on every pass never converges under ArgoCD.
The app sits `OutOfSync`, `selfHeal` re-applies it, and any workload carrying a `checksum/`
annotation rolls its pods — on every sync, forever.

---

## The failure this exists for

Three Harbor `Deployment`s in my homelab reached **revision 658 over 729 days**. Nothing was
wrong with the cluster. The chart did this, which is an extremely common idiom:

```gotemplate
password: {{ .Values.auth.password
             | default (lookup "v1" "Secret" .Release.Namespace "creds").data.password
             | default (randAlphaNum 32) }}
```

Under `helm install`, `lookup` finds the existing Secret and the password is stable.
Under ArgoCD, `lookup` returns `{}` — repo-server has no cluster access — so the third branch
fires on **every render**. A new password, every sync.

The same chart also rewrote a Postgres superuser password until it no longer matched the
database. That `StatefulSet` had no `checksum/` annotation, so **nothing restarted and nothing
alerted.** It was silently broken for two years.

You cannot catch this with `argocd app diff` either —
[from its own docs](https://argo-cd.readthedocs.io/en/stable/user-guide/commands/argocd_app_diff/):

> *Kubernetes Secrets are ignored from this diff.*

The objects where this bug lives are exactly the ones ArgoCD will not show you.

**Verify it yourself in ten seconds**, on the most-pulled chart in the ecosystem:

```sh
helm template pg oci://registry-1.docker.io/bitnamicharts/postgresql | grep postgres-password
helm template pg oci://registry-1.docker.io/bitnamicharts/postgresql | grep postgres-password
```

Two different values. Five renders give five passwords.

---

## Status

**Early, and not yet released.** The comparison engine, manifest parsing, path addressing and
chart-reference handling are built and tested — and so is a working CLI: `idem <path>` discovers
charts, renders each of them more than once, compares the results and prints the verdict, with
the exit codes below.

Three-engine verdicts work: `idem` reports what a finding means under ArgoCD, Flux and Helm,
and tells you whether you are looking at a chart defect or an ArgoCD limitation.

The `ignoreDifferences` emitter works too: one pasteable block per run, carrying every
differing field even where the display above it elides some.

`idem` also reads your ArgoCD `Application` / `ApplicationSet` and Flux `HelmRelease`, so a
finding you have already covered with an `ignoreDifferences` block is reported as handled rather
than shouted about again — and it will not re-emit config you already have.

The static analyzer works too: a chart that calls a non-deterministic function but rendered
identically is reported as a **potential** finding — its own section, never counted, never
fatal — because the failure that motivated `idem` was a pin that silently stopped applying.

All four output formats work: `text`, `json`, `markdown` and `github`.

Dependency resolution works as described below, without ever writing to your repository.

The `--new-from-rev` / `--new-from-merge-base` ratchet works too.

Not yet built: `--cluster` and `doctor`. Engine
auto-detection is not built either, so `--engine` shows all three unless you narrow it, and a
chart rendered straight from a registry cannot be scanned for `lookup` yet — that reports
`unknown` and says so. `action.yml` downloads a release that does not exist yet. So most of
what follows is still the intended interface — a specification, not a demo.

Three-engine verdicts (ArgoCD, Flux, Helm) are v1 scope, not a later addition. They are the
reason the tool exists.

## Install

Once released, either of:

```sh
brew install pcanilho/taps/idem
go install github.com/pcanilho/idem@latest
```

`idem` shells out to whichever `helm` is on your `PATH`, and prints which one it used —
results can depend on it.

---

## Usage

One command, and the verb is optional:

```sh
idem ./charts                 # check (default)
idem diff a.yaml b.yaml       # compare two renders you produced yourself
```

### The common case

Most runs find nothing, because you fix a chart once and it stays fixed:

```console
$ idem ./charts
✓ All 10 charts render consistently under ArgoCD.
  helm 4.2.4 · 2 rounds
```

It names the helm binary and round count on purpose. A silent pass that does not say what it
checked is a pass you cannot trust — and ArgoCD 3.5 swapped Helm 3.19 for 4.2 underneath
everybody.

### When something is wrong

One line per finding, grouped by the template that produced it, and **one** remediation block
at the end — so you paste once, not N times:

```console
$ idem ./charts

  home/templates/secrets.yaml
    Secret/home-ollama-secrets   .data.WEBUI_SECRET_KEY   rolls 2 Deployments

  lab/templates/database.yaml
    Secret/lab-harbor-postgres   .data.password           silent — no checksum
    Secret/lab-harbor-postgres   .data.registry-token     silent — no checksum

  2 of 10 charts will churn under ArgoCD; 1 could not be rendered.
  helm 4.2.4 · 2 rounds · run with --strict to gate on this

  Add to your ArgoCD Application to stop the churn:

    spec:
      ignoreDifferences:
        - kind: Secret
          name: home-ollama-secrets
          jsonPointers: [/data/WEBUI_SECRET_KEY]
        - kind: Secret
          name: lab-harbor-postgres
          jsonPointers:
            - /data/password
            - /data/registry-token
      syncPolicy:
        syncOptions: [RespectIgnoreDifferences=true]

  exit 2 — a chart could not be rendered
```

The right-hand column is the whole product in three words. `rolls 2 Deployments` and
`silent — no checksum` are the difference between an annoyance and a credential that has been
drifting from your database for two years.

Grouping is free and exact: `helm template` marks every document with a `# Source:` comment.
Input without one — `argocd app manifests` output has lost them — groups under
`(source unknown)`. Absent is reported as absent, never guessed.

### Before you adopt someone else's chart

```console
$ idem oci://registry-1.docker.io/bitnamicharts/postgresql --engine all

  postgresql/templates/secrets.yaml
    Secret/pg-postgresql   .data.postgres-password

      argocd    CHURNS     every sync, forever; workloads with a checksum/ annotation roll
      flux      unknown    chart uses `lookup` (common/_secrets.tpl:103) — may guard this value
      helm      unknown    same

  This chart will churn under ArgoCD. Under Flux and Helm: unknown.
  helm 4.2.4 · 2 rounds
```

Add `--cluster` and those `unknown`s become measured facts — see
[Three engines](#three-engines-three-different-answers).

### Flags

That is the whole surface:

```
  -f, --values      values file, repeatable                     (as helm)
      --set         set a value, repeatable                     (as helm)
      --rounds      renders to compare                        (default 2)
      --engine      argocd, flux, helm, all, or auto
                    (default: auto-detected; all three when undetectable)
      --strict      exit non-zero on findings         (default: report only)
      --helm        helm binary to render with   (default: first on PATH)
      --cluster     resolve lookup and capabilities against the current kube context
      --kube-context  which context to use              (default: current)
      --jobs        renders to run at once        (default: number of CPUs)
      --dependency-update  resolve missing deps in place, not a temp dir
      --no-deps     never fetch dependencies      (airgapped / reproducible CI)
      --new-from-rev REV          report only findings in charts changed since REV
      --new-from-merge-base REF   same, against the merge base with REF
      --repo        chart repository URL, as helm's --repo
      --chart-version  chart version, as helm's --version
  -o                text, json, markdown or github        (default text)
  -v                expand every finding
      --version     print idem's version
```

The chart version is `--chart-version`, not `--version`, which is the one place `idem`
deliberately does not mirror helm. `idem --version` is the flag every command-line tool has, and
answering it with a chart's version would be a surprise in the one place nobody expects one.

### Output formats

`-o text` is the default above. `-o json` is the machine-readable contract, and `-o markdown`
is shaped for a pull-request comment:

````console
$ idem ./charts -o markdown
````

```markdown
### idem — 2 of 10 charts will churn under ArgoCD

| chart | object | field | consequence |
|---|---|---|---|
| `home` | `Secret/home-ollama-secrets` | `.data.WEBUI_SECRET_KEY` | rolls 2 Deployments |
| `lab` | `Secret/lab-harbor-postgres` | `.data.password` | silent — no checksum |
| `lab` | `Secret/lab-harbor-postgres` | `.data.registry-token` | silent — no checksum |

<details>
<summary>Fix — add to your ArgoCD Application</summary>

    spec:
      ignoreDifferences:
        - kind: Secret
          name: home-ollama-secrets
          jsonPointers: [/data/WEBUI_SECRET_KEY]
      syncPolicy:
        syncOptions: [RespectIgnoreDifferences=true]

</details>

<sub>helm 4.2.4 · 2 rounds · 1 chart could not be rendered</sub>
```

The fix is collapsed because it is long and only some readers need it, and the table survives
GitHub's renderer without alignment tricks. Piping that into `gh pr comment --body-file -` is
the whole CI integration:

```yaml
- run: idem ./charts --new-from-merge-base ${{ github.base_ref }} -o markdown > /tmp/idem.md
- run: gh pr comment ${{ github.event.number }} --body-file /tmp/idem.md
  if: ${{ hashFiles('/tmp/idem.md') != '' }}
```

**No HTML, CSV or SARIF in v1.** JSON covers every machine consumer, markdown covers the human
one that matters, and each additional format is a rendering to maintain forever.

### GitHub Actions

`idem` ships an action, and it is thin on purpose — the tool knows how to emit annotations, the
action only installs and runs it:

```yaml
- uses: pcanilho/idem@v1
  with:
    args: ./charts --new-from-merge-base ${{ github.base_ref }}
    helm-version: 4.2.1       # match whatever your ArgoCD runs
```

The action lives in this repo rather than a separate one, is composite rather than Docker (no
image pull), and pins nothing for you: `version: latest` resolves at run time and says so in the
log, because a new release silently changing your CI result is the same class of surprise this
tool exists to report.

`-o github` emits workflow commands (`::error file=…,line=…::`), so findings appear **inline on
the diff** in Files Changed — no token, no API calls, no `pull-requests: write` permission.

**Which findings can be pinned to a line, and which cannot.** This is worth stating plainly,
because a tool that annotates the wrong line is worse than one that annotates nothing:

| Finding | Repo location | Annotation |
|---|---|---|
| Floating dependency | `Chart.yaml`, the dependency's own line | **exact line** |
| Potential (static scan) | the template, at the function call | **exact line** |
| Observed, local chart | the template, from `# Source:` — no line | **file-level** |
| Observed, remote chart | nothing in the repo | **summary only** |

`helm template` marks each document with the template that produced it but carries no line
numbers, and connecting a rendered field back to the template line that emitted it is the
attribution problem `idem` deliberately does not attempt. So an observed finding annotates the
file, not a guessed line.

Findings with no repo location — anything from an OCI or `--repo` chart — are not dropped; they
go in the summary comment instead:

```yaml
- run: idem ./charts -o markdown > /tmp/idem.md
- run: gh pr comment ${{ github.event.number }} --body-file /tmp/idem.md
```

Use both together: annotations for what has a line, one comment for the rest. GitHub also caps
how many annotations it will render per run, so `-o github` prints the cap it hit rather than
letting findings disappear silently.

**There is no rules file and no exceptions file, deliberately.** Suppression is something you
need *after* you have run a tool and disagreed with it — nobody has exceptions on day one. If
you need to filter or gate programmatically, `-o json` is the seam:

```sh
idem ./charts -o json | jq '.findings[] | select(.consequence == "rolls")'
idem ./charts -o json | conftest test -
```

Let OPA be the policy engine. `idem` reports facts.

---

## Dependencies

A chart with unresolved subchart dependencies cannot render. `idem` handles that without
failing and without touching your working tree:

1. **Render as-is first.** If your repo vendors its `charts/*.tgz` — as a GitOps monorepo
   usually does — this is the whole story and costs nothing.
2. **Otherwise resolve in a temp directory.** Copy the chart out, `helm dependency build`
   there, render, discard. Chart source is small (Bitnami's postgresql is ~250K of templates),
   so this is cheap.
3. `--dependency-update` resolves **in place** instead, if you would rather populate helm's
   cache and your own `charts/` directory.
4. `--no-deps` never fetches. Charts with missing dependencies become `unevaluable` and exit
   `2`, with the `helm dependency build` command you need. For airgapped builds, or when you
   want a run to be byte-reproducible.

```console
$ idem ./charts
✓ All 10 charts render consistently under ArgoCD.
  helm 4.2.4 · 2 rounds · 8 vendored, 2 resolved in a temp dir
```

`idem` never writes to your repository unless you pass `--dependency-update`. A linter that
leaves your `git status` dirty is a linter people stop running.

---

## CI: only fail on what you just changed

Adding any linter to an existing estate finds a pile of pre-existing issues, and a permanently
red pipeline gets deleted rather than fixed. `idem` borrows golangci-lint's answer — git
revisions, not a baseline file:

```yaml
- run: idem ./charts --new-from-merge-base ${{ github.base_ref }} --strict
```

```console
$ idem ./charts --new-from-merge-base main --strict

  home/templates/secrets.yaml
    Secret/home-ollama-secrets   .data.WEBUI_SECRET_KEY   rolls 2 Deployments

  1 of the 2 charts changed since main will churn under ArgoCD.
  7 pre-existing findings not shown — drop the flag to see them.
  exit 1
```

Nothing is stored and nothing is suppressed: there is no baseline file to maintain, no
generated allowlist to review, and dropping the flag always shows you everything. The
granularity is a **chart**, not a line — a finding belongs to a rendered object, not to a
source line — so a chart with any changed file is fully re-examined.

---

## Your git may not describe what is running

A `Chart.yaml` dependency can declare a range. Rendering resolves it against the repository
index *at render time*, so nothing in git records which version was actually used:

```yaml
dependencies:
  - name: romm
    version: "=>9.2.9"
```

Helm stamps the resolved version on every object it renders (`helm.sh/chart: romm-19.4.0`), so
`idem` can compare what you declared against what you are running — a label read, no rendering:

```console
$ idem ./charts/home --cluster

  — floating dependencies · not counted, not fatal —

  19 of 19 dependencies are running above their declared floor:

    romm            =>9.2.9     running 19.4.0     10 major versions above
    lidarr          =>23.0.2    running 29.7.3
    jackett         =>22.0.1    running 27.7.25
    …

  Git does not record these versions. A rebuild, a repo-server cache expiry, or any
  re-resolve deploys whatever is newest at that moment — which need not be what is
  running now.
```

**This is reported, never judged.** Floating ranges are how you get auto-update, and plenty of
people choose them deliberately — gitignoring the lock file precisely so resolution is not
frozen. `idem` cannot know whether that is your intent, so the finding is informational: it does
not count toward the totals and never affects the exit code, exactly like a potential finding.

If you *do* want to enforce pinning, that is a policy question rather than a correctness one:
`idem -o json | conftest test -`.

## `idem doctor` — find it in a cluster you already run

Everything else predicts *"this will churn"*. `doctor` finds *"this has been churning for two
years"*. No chart, no git, no rendering — two cluster queries:

```console
$ idem doctor

  Scanning 56 workloads for sync churn…

   rev   per day   age    workload
   660      0.89   743d   lab/lab-harbor-registry     checksum/secret + 3 more
   660      0.89   743d   lab/lab-harbor-jobservice   checksum/secret + 3 more
   660      0.89   743d   lab/lab-harbor-core         checksum/secret + 2 more
   594      0.74   803d   home/home-ollama            checksum/secrets + 8 more
   345      0.50   693d   home/home-romm              checksum/secrets + 8 more

  Cluster median is 0.14 rollouts/day. These 5 carry a checksum/ annotation and
  roll far more often than their images change — consistent with a Secret that
  is regenerated on every sync.

  Confirm the cause:   idem <their chart> --cluster
```

It also reads the delivery chain and attributes divergence that the engines report but do not
explain:

```console
$ idem doctor --namespace lab

  ArgoCD says lab-app has 1 resource OutOfSync:

    Secret/lab-gitea-mirror
      applied with 0 data keys; live has 4
        BETTER_AUTH_SECRET  ENCRYPTION_SECRET  GITEA_TOKEN  GITHUB_TOKEN
      written after apply by external-secrets
        (label reconcile.external-secrets.io/managed)

      Not a chart problem and not an ArgoCD problem: two owners for one object.
      Stop ArgoCD managing the data:
        ignoreDifferences:
          - kind: Secret
            name: lab-gitea-mirror
            jsonPointers: [/data]
```

`argocd app diff` will not show you that one — it ignores Secrets, which is where this class of
problem lives. `idem` reads the object's own record of what was applied and compares it to what
is there now, so the answer needs no chart, no rendering and no dry-run.

**This is triage, not proof.** A high revision count also comes from deploying often. What
makes it a signal is the combination — rolling far above the cluster median *and* carrying a
`checksum/` annotation derived from a Secret. `doctor` ranks suspects; `idem <chart> --cluster`
establishes the cause.

---

## What a cluster connection adds

`--cluster` is opt-in and read-only. It never applies, creates, updates, or owns anything.

**1. `lookup` resolves, so `unknown` becomes measured.** Covered under
[Three engines](#three-engines-three-different-answers).

**2. Real capabilities.** The cluster's actual `--api-versions` and `--kube-version` are passed
through, matching what ArgoCD does — so charts gated on `.Capabilities.APIVersions.Has` render
the way they will for you, not against Helm's defaults.

**3. The true effective values.** Reading the live `Application` or `HelmRelease` gets values
that git cannot supply: Flux `valuesFrom` references and `postBuild` substitutions resolve
in-cluster. For Flux this is often the difference between a qualified verdict and a real one.

**4. Drift that has nothing to do with your chart.** A chart can be perfectly deterministic and
still never converge, because something else changes the object — either as it is applied
(webhooks, API-server defaulting) or afterwards (External Secrets, cert-manager, operators).
Those are different problems: a dry-run reproduces the first and is blind to the second, which
is only visible by comparing the live object to its own `last-applied` record. `idem` does
both:

```console
$ idem ./charts/home --cluster

  home/templates/service.yaml
    Service/home-plex   deterministic, but the cluster rewrites it

      .spec.clusterIP           cluster assigns      172.17.0.0
      .spec.sessionAffinity     cluster defaults     None
      .spec.ports[0].protocol   cluster defaults     TCP

      ArgoCD normalises most API-server defaulting, so this is usually benign.
      Mutating webhooks that touch the objects ArgoCD manages are not.
```

`idem` renders the chart, asks the API server what it would actually store
(`--dry-run=server`, nothing persisted), and compares the two with the same engine it uses for
everything else. Two honest caveats: ArgoCD already normalises most plain defaulting, and
pod-level injectors — Istio sidecars, the Vault agent — mutate *Pods*, not Deployments, so they
do not cause Deployment-level drift. `idem` says which mutating webhooks actually match the
objects in question rather than implying every webhook is a problem.

---

## Chart references

Three of the four forms need no setup at all:

| Form | Example | Setup |
|---|---|---|
| local | `idem ./charts/home` | none |
| OCI | `idem oci://registry-1.docker.io/bitnamicharts/postgresql` | none |
| explicit repo | `idem postgresql --repo https://charts.example.com` | none |
| repo alias | `idem bitnami/postgresql` | needs `helm repo add` first |

The last one bites: `bitnami/postgresql` is a *repo alias*, and on a machine that has never run
`helm repo add bitnami` it fails with `Error: repo bitnami not found`. `idem` detects that and
prints the command you need instead of passing the error through.

**Any OCI registry works** — ECR, GHCR, Harbor, Artifact Registry, ACR, localhost. `idem`
matches the `oci://` scheme and hands the reference to `helm`; it has no per-registry knowledge
and needs none.

**Authentication is delegated, deliberately.** `idem` never sees a credential:

```sh
helm registry login ghcr.io -u "$USER" --password-stdin <<< "$GITHUB_TOKEN"
idem oci://ghcr.io/acme/charts/api

# or reuse existing docker credential helpers, including in CI
HELM_REGISTRY_CONFIG=~/.docker/config.json \
  idem oci://123456789012.dkr.ecr.eu-west-1.amazonaws.com/charts/api
```

---

## Three engines, three different answers

This is the point of `idem`. The same chart, the same finding, means something different
depending on what reconciles it:

| | renders with | `lookup` resolves? | re-renders |
|---|---|---|---|
| **argocd** | `helm template`, no cluster access | **never** — always `{}` | every reconcile |
| **flux** | a real install/upgrade via the Helm SDK | yes | only when chart or values change |
| **helm** | a real install/upgrade | yes | only on `helm upgrade` |

`idem` picks a lens by looking at the directory — an ArgoCD `Application` means ArgoCD's
answer, a Flux `HelmRelease` means Flux's. With no signal either way it shows all three,
because that is exactly when you are evaluating a chart and want to know.

There is no `--type` flag for input kinds and there never will be. If you have to tell the tool
what it is looking at, the tool should have looked.

**Flux is immune to render-side churn, not to the other two.** `lookup` resolving means a
non-deterministic chart is genuinely not a Flux problem. But engine-side rewrites and cluster
mutation hit Flux exactly as hard — and Flux has no continuous divergence surface to tell you:
a `HelmRelease` reports `Ready: True` once the release succeeded, and `driftDetection.mode`
defaults to `disabled`. **Quieter, not safer** — which is why `--cluster` matters more for Flux
users, not less.

**A chart with no `lookup` at all** is a different verdict everywhere — and a different fix:

```console
  acme/templates/secrets.yaml
    Secret/acme-api   .data.session-key

      argocd    CHURNS     every sync
      flux      CHURNS     on every chart or values change
      helm      CHURNS     on every `helm upgrade`

      No `lookup` anywhere in this chart, so nothing can stabilise this value. This is a
      chart defect rather than an ArgoCD limitation — worth reporting upstream. Pin
      `auth.sessionKey` meanwhile.
```

Telling that apart from the `lookup` case is the single most useful thing `idem` does. It
answers the only question you actually have: **do I file an upstream issue, or do I add an
`ignoreDifferences` block?**

### `--cluster`: turn `unknown` into a fact

`helm template` defaults to `--dry-run=client` and resolves `lookup` to `{}` — exactly what
ArgoCD's repo-server does. `--dry-run=server` resolves it for real, which is what Flux and
`helm upgrade` do. So `idem` can answer by observation instead of argument:

```console
$ idem oci://registry-1.docker.io/bitnamicharts/postgresql --cluster

      argocd    CHURNS     every sync — repo-server renders without cluster access
      flux      stable     lookup resolves; value identical across renders (observed)
      helm      stable     same

      `lookup` finds Secret/pg-postgresql in namespace `default`. ArgoCD will not:
      it renders with `helm template`, which resolves lookup to {} by construction.
      That single difference is the entire bug.
```

**`idem` never writes to your cluster.** `--dry-run=server` is a render-time query, not an
apply. No create, no update, no server-side apply, no ownership of anything. Deployment belongs
to your GitOps engine.

For Flux this is closer to mandatory than optional — a `HelmRelease` often defers its values to
cluster-resident `valuesFrom` refs. See
[design notes §1](docs/design.md#1-what-idem-is-actually-judging).

---

## Exit codes

`idem` reports; it does not fail your build unless you ask it to.

| Code | Meaning |
|---|---|
| `0` | Ran successfully. Findings are printed but not fatal. |
| `1` | Findings, **and** `--strict` was passed. |
| `2` | A chart could not be rendered, or `idem` itself failed. **Always fatal.** |

Exit `2` is not negotiable even without `--strict`. A chart that silently fails to render and
is then skipped is the same class of bug this tool exists to catch.

```yaml
- run: idem ./charts --strict
```

---

## What it does not do

- It does not talk to your cluster unless you pass `--cluster`, and even then only to render —
  never to apply, create, update, or own anything.
- It does not reconstruct your delivery pipeline — no kustomize overlays, no `postBuild`
  substitution, no post-renderers. It renders what you point it at and names what it could not
  see.
- It does not prove a chart is free of secrets. It proves the output is *stable*, which is the
  property ArgoCD actually needs.
- It does not claim a workload's `checksum/` annotation was derived from a given Secret by
  inspection — that value is a hash and cannot be inverted. It reports that the two **change
  together across renders**, which is an observation, not a guess.
- It renders twice **back to back**, so it cannot see non-determinism that unfolds over *time*:
  an unpinned dependency range (`^1.0`), a floating `:latest` tag, a re-published chart version.
  A clean run means "this renders consistently right now, with this helm" — not "this chart is
  pinned or reproducible next month".
- Where it cannot establish something, it says `unknown` rather than guessing.

---

## Design notes

The reasoning, the limits and the evidence live in **[docs/design.md](docs/design.md)**:
what `idem` is judging and why it is a release rather than a chart, how each verdict is
reached and how certain it is, what `idem` actually runs for each engine, why this is an
ArgoCD problem specifically (with receipts), and why it shells out to `helm`.

## License

Apache-2.0
