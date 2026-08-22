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

## Supported versions

Pre-1.0 and unreleased. Fixes land on `main`; there are no backports yet.

## Reporting a vulnerability

Please report privately through
[GitHub Security Advisories](https://github.com/pcanilho/idem/security/advisories/new) rather
than opening a public issue.

Include what you can reproduce and what an attacker would gain. You will get an acknowledgement
within a week.
