# Security

## What `idem` does with your cluster and your repository

Worth stating plainly, because a tool that reads Helm charts and Kubernetes Secrets deserves the
question:

- **It never writes to your cluster.** `idem` shells out to `kubectl` and makes exactly two
  kinds of call: `kubectl get … -o json`, and `kubectl apply --dry-run=server`, which the API
  server evaluates and discards without storing. There is no create, update, patch or delete
  anywhere in the codebase.
- **Bare `idem doctor` reads no Secrets.** It makes two cluster-wide reads —
  `deployments,statefulsets,daemonsets` and `applications.argoproj.io` — and nothing else.
- **`idem doctor --namespace <ns>` additionally reads `secrets` and `configmaps`, in that one
  namespace.** That is the privilege-sensitive call, and it is opt-in: finding what a controller
  writes after apply means comparing live objects against their own `last-applied` record, and
  for Secrets there is no other evidence. It is scoped to the namespace you name — `kubectl get
  -n <ns>`, never `--all-namespaces`.
- **It never writes to your repository.** Subchart dependencies resolve in a temporary directory
  that is removed afterwards, unless you explicitly pass `--dependency-update`.
- **It sends nothing anywhere.** No telemetry, no analytics. `idem` imports no HTTP client at
  all — the only outbound traffic is `helm` fetching a chart you asked for, using your existing
  helm and registry configuration.
- **It runs three external binaries and no others**: `helm` to render, `kubectl` for `--context`
  and `doctor`, and `git` for `--new-from-rev`. All are taken from your `PATH`; `--helm` lets you
  pin which one.
- **It reads Secrets, and it prints field names.** `idem` compares rendered objects, so Secret
  data passes through it. Output names *paths* (`.data.password`) and never prints Secret values
  — but `-o json` output is still derived from your rendered manifests, so treat it with the same
  care as the manifests themselves.

## Fixed

- **Argument injection into `git` via `--new-from-rev`** (2026-08-22, pre-release, never
  published). `git diff --name-only <rev>` let a value beginning with `-` be read by git as an
  option, and `git diff --output=FILE` truncates FILE — so
  `idem --new-from-rev=--output=/path/to/anything` destroyed that file while printing an ordinary
  report and exiting 0. A value naming a *path* also silently disabled the ratchet, hiding every
  finding behind "No charts changed since …" and exit 0. Revisions are now validated with
  `git rev-parse --verify` and every git invocation passes `--end-of-options`.

- **Script injection through the GitHub Action's `args`** (2026-08-25, never in a published
  release; `v0.1.0` was rebuilt with the fix before anyone could consume it).
  `action.yml` interpolated `${{ inputs.args }}` directly into a `run:` block, so an `args` value
  containing `$(...)` or a backtick executed on the runner. Wiring `args` from a pull-request
  title or a branch name — an ordinary thing to do — was therefore arbitrary command execution in
  the calling repository. Inputs now reach the script through `env:`, where bash expands them as
  data and does not re-parse the result.

## Supported versions

Pre-1.0 and unreleased. Fixes land on `main`; there are no backports yet.

## Reporting a vulnerability

Please report privately through
[GitHub Security Advisories](https://github.com/pcanilho/idem/security/advisories/new) rather
than opening a public issue.

Include what you can reproduce and what an attacker would gain. You will get an acknowledgement
within a week.
