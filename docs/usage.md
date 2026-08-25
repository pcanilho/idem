# Usage

```
idem [chart] [flags]     check a chart, or every chart under a directory
idem diff a.yaml b.yaml  compare two renders you produced yourself
idem doctor [flags]      ask a cluster you already run what keeps rolling
```

## Flags

| Flag | What it does |
|---|---|
| `-f`, `--values` | values file, repeatable |
| `--set` | set a value, repeatable |
| `--rounds` | how many renders to compare (default 3) |
| `--strict` | exit 1 when something will churn |
| `-v` | expand every finding instead of capping each at five fields |
| `-o` | `text`, `json`, `yaml`, `markdown` or `github` (`diff` and `doctor` take the first three) |
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

Flags may come before or after the chart path. One dash or two works for every flag, so `--strict`
and `-strict` are the same thing, because `idem` parses with Go's standard library rather than
taking a second dependency for it. There is no short-flag clustering: `-vs` is not `-v -s`.

## Output formats

`-o json` is the machine-readable contract, so you can gate on it however you like. `-o yaml` is
the same document, for when the next thing in the pipe reads YAML:

```sh
idem ./charts -o json | jq '.findings[] | select(.consequence == "rolls")'
idem ./charts -o json | conftest test -
idem ./charts -o yaml | yq '.remediation[] | select(.engine == "argocd")'
idem doctor -o json   | jq '.suspects[] | select(.perDay > 0.5) | .name'
idem diff a.yaml b.yaml -o json | jq '.findings[].paths[].pointer'
```

`diff` and `doctor` take `text`, `json` and `yaml`. They refuse `markdown` and `github`, which
describe a chart in a pull request rather than a cluster.

## ArgoCD, Flux and Helm

"Is this chart broken?" has no single answer, because the three engines do not render it the same
way. ArgoCD's repo-server runs `helm template` with no cluster access, so `lookup` finds nothing
and a value guarded by it churns on every sync. Flux's helm-controller does a real install, and
`helm upgrade` talks to the cluster; under both, that same value resolves and holds still.

A chart using `lookup` is therefore *correct Helm* that cannot work under ArgoCD. `idem` answers
per engine rather than per chart, so it tells you whether the fix belongs in your Application or
upstream in the chart.

## Rendering against a cluster

Without a cluster, `idem` says `unknown` for Flux and Helm rather than guessing. Give it one and
those become measured facts:

```sh
idem ./charts --context=              # your current kube context
idem ./charts --context=prod          # a named one
```

`--context` is opt-in and read-only. It renders through the API server (`--dry-run=server`), so
`lookup` resolves and your real cluster capabilities are used. It never applies anything.

With a cluster it also reports `the cluster rewrites these on admission`. Most of that is
harmless API-server defaulting, but a mutating webhook writing into a field your chart also sets
is a drift loop that no amount of rendering reveals.

## `idem diff`

The comparison engine on its own, with no helm, no network and no cluster. This is also how you
point `idem` at kustomize:

```sh
kustomize build overlays/prod > a.yaml
kustomize build overlays/prod > b.yaml
idem diff a.yaml b.yaml
```
