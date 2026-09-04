# Limits

- **It renders twice, back to back**, so it cannot see non-determinism that unfolds over *time*:
  an unpinned dependency range, a floating `:latest` tag, a re-published chart version. A clean
  run means "this renders consistently right now, with this helm", not "this chart is pinned
  forever".
- **It does not reconstruct your delivery pipeline.** No kustomize overlays, no `postBuild`
  substitution, no post-renderers. It renders what you point it at, and names in the output
  whatever it could not resolve rather than rendering defaults and calling that an answer.
  Supply those values yourself with `-f` or `--set` and they stop being named: a flag you
  typed is taken as the answer, so the caveat clears and `--strict` can reach green.
- **It reads, never writes.** Not your cluster (`--context` renders through the API server;
  `doctor` only does `kubectl get`), and not your repository (subcharts resolve in a temp
  directory unless you pass `--dependency-update`).
- **It proves output is stable, not that a chart is safe.** Stability is the property your engine
  needs; it is not a secrets scan or a policy check.
- **Not every finding comes with config.** A list that only *reorders*, holding the same elements
  in a different order, churns just as much, but neither ArgoCD nor Flux can ignore ordering
  without also ignoring the list's contents. `idem` says so and points at the fix in the chart,
  rather than handing you a block that would hide real changes along with the noise.
- **It skips `type: library` charts, and says how many.** Helm refuses to render one at all
  (*"library charts are not installable"*), so there is nothing to compare. They are counted on
  the provenance line rather than dropped in silence, and they no longer fail the run, which they
  used to.

**It needs `helm` on your `PATH`, and which one matters.** `idem` renders with whatever `helm`
it finds. Pin it with `--helm`, and pin it in CI to whatever your ArgoCD runs, because ArgoCD
3.4 shipped Helm 3.19 and 3.5 ships Helm 4.2. The version used is printed on every run.

---

## Compared to other tools

| Tool | Compares | Finds a chart that churns? | Writes the fix? |
|---|---|---|---|
| **`idem`** | one release against **itself**, twice | **yes** | **yes** |
| `argocd app diff` | desired vs live, once, *and ignores Secrets* | no | no |
| `helm diff` | two chart directories, or a release against an upgrade | `helm diff local` on one directory twice shows a raw diff, with no verdict and no engine | no |
| `helm unittest` | this render vs a committed snapshot | goes red, but re-baselining hides it | no |
| kubeconform, kube-score, polaris | schema and policy on one render | no | no |
| conftest / OPA | policy on one render | no | no |

Everything else is built to compare two *different* things. `helm diff local` is the one that can
be pointed at a single chart twice, and then it shows you the raw diff, which is the easy half.
The hard half is what idem is: knowing that a differing field means churn *under your engine*,
telling a regenerated value apart from a reordered list, and writing config that stops it without
hiding anything else.
