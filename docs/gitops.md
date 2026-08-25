# Your ArgoCD and Flux config

`idem` reads the ArgoCD `Application` / `ApplicationSet` and Flux `HelmRelease` in your
repository. That tells it three things:

- **what you already suppress**: a finding covered by your own `ignoreDifferences` is shown as
  handled, not shouted about again, and it does not fail `--strict`.
- **what your chart is rendered with**: values, release name, and namespace come from the
  manifest that deploys it, because a chart rendered with no values is a release nobody runs.
  ApplicationSet generators that read the repository are expanded, one release per element.
- **which engines you use**, so you only see verdicts and fix blocks for engines you run.

One case is worth calling out. An `ignoreDifferences` block with `selfHeal: true` and no
`RespectIgnoreDifferences=true` hides the diff while re-applying the object anyway. `idem`
reports that as a trap rather than as handled, because you believe it is fixed and it is not.

---

## `idem doctor`

Checking a chart predicts churn. `idem doctor` finds churn that has already happened, with no
chart needed:

```sh
idem doctor                      # what keeps rolling, and who owns it
idem doctor --namespace lab      # what is being written after apply
```

It ranks workloads by how often they roll against the cluster's own median, names the Application
or HelmRelease that owns each, and resolves that to a chart path, so the last line is a command
you can run. It calls that triage rather than proof: deploying often looks the same from here.

`--namespace` asks the other question, which fields were written *after* the apply. It separates
`applied absent, live set` from `applied and live differ`, and names the controller that did it
where the object carries evidence of one.
