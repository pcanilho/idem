# CI and pre-commit

Findings are informative by default. `--strict` turns them into a failing build:

```yaml
- uses: pcanilho/idem@v0.1.0
  with:
    args: ./charts --strict
    helm-version: v3.19.2    # optional, see below
```

`version:` defaults to the ref you used the action **at**, so `@v0.1.0` installs the v0.1.0
binary and a later idem release cannot change your result without you changing the tag. A ref
that names no release — a branch, a commit SHA, or a local `uses: ./` — has nothing to derive
from, so it resolves `latest` and says so in the log; if you pin this action to a SHA, set
`version:` alongside it. Pass `version: latest` if you would rather track the newest.

GitHub's Ubuntu runners already ship Helm, so the action renders with that unless you say
otherwise, and installs nothing. Set `helm-version:` to whatever your ArgoCD runs and it will
install exactly that. It is the same advice as `--helm`, for the same reason: the renderer is an
input, and a finding produced under a helm you do not run is a finding about someone else's
cluster.

## Inputs

| Input | What it does |
|---|---|
| `args` | Everything after `idem`, split on whitespace. An argument containing a space cannot be expressed; globs expand on the runner. |
| `version` | Release to install. Defaults to the ref you used the action at. |
| `output` | `github` (default), `text`, `json`, `yaml` or `markdown`. Passed as `-o`, so do not also put it in `args` — the action refuses that rather than overriding you silently. |
| `report-file` | Also write the report to this workspace-relative path, for posting as a pull-request comment (below). |
| `working-directory` | Directory to run in. Defaults to the workspace root. |
| `helm-version` | Helm to install and render with. Empty uses whatever the runner already has. |

## Outputs

| Output | What it is |
|---|---|
| `exit-code` | `0`, `1` or `2`. Set `continue-on-error: true` on the step to read it rather than be failed by it. |
| `report-file` | The path `report-file` was written to, or empty. |

There is deliberately no finding **count**. `-o` takes one format, so producing a number
alongside a readable report would mean rendering every chart twice — and a count inferred from
the exit code would be a guess.

## Annotations and comments

`-o github` emits workflow commands, so findings appear as annotations on the run — and inline in
Files Changed wherever `idem` can place a line — with no token and no API calls. To post one
summary comment instead:

```yaml
permissions:
  contents: read           # actions/checkout
  pull-requests: write     # the comment step needs it; `-o github` does not
# ...
- uses: pcanilho/idem@v0.1.0
  with:
    args: ./charts
    output: markdown
    report-file: idem.md
- run: gh pr comment ${{ github.event.number }} --body-file idem.md
  if: ${{ hashFiles('idem.md') != '' }}
  env:
    GH_TOKEN: ${{ github.token }}
```

Declaring `permissions:` at all zeroes every scope you do not list, which is why `contents: read`
is there: without it `actions/checkout` fails before idem is ever reached.

A clean run writes **nothing**, so the guard is what stops a comment saying everything is fine on
every pull request that touches a chart. Keep the path **inside the workspace**, not `/tmp`:
`hashFiles` only sees files under `GITHUB_WORKSPACE` and returns an empty string for anything
else, so an absolute path makes the guard permanently false and the comment is never posted at
all. The action refuses an absolute or escaping `report-file` for that reason. `steps.<id>.outputs.report-file`
gives the path back when your workflow computes it rather than writing it literally.

## The ratchet

**Day one on a real estate will find things.** That is the point, and it is also why the first
run should not be the gate. Report only what your branch changed:

```yaml
- uses: actions/checkout@v7
  with:
    # --new-from-merge-base resolves the base ref with git, and the default
    # shallow checkout has neither the ref nor the history. Without this idem
    # exits 2 before it renders anything.
    fetch-depth: 0
- uses: pcanilho/idem@v0.1.0
  with:
    args: ./charts --new-from-merge-base ${{ github.base_ref }} --strict
```

A permanently red pipeline gets switched off, so the ratchet exists to keep it green from day
one. It filters *findings* only. A chart that will not render at all is still reported, because
that is a gap in what was checked rather than a finding about it.

## Verifying the download

The action checks the archive against the release's `checksums.txt`, which catches a truncated or
corrupted download but is not tamper protection — both files ship in the same release. Releases
also carry build provenance, so authenticity is available to anyone who wants it:

```sh
gh attestation verify idem_linux_amd64.tar.gz --repo pcanilho/idem
```

The action does not run that itself, because it would put `gh` back on the critical path — and
`gh` is absent from `container:` jobs and most self-hosted runners, which is exactly why the
install step uses `curl` instead.

## pre-commit

`idem` ships a hook for [pre-commit](https://pre-commit.com) and
[prek](https://github.com/j178/prek):

```yaml
repos:
  - repo: https://github.com/pcanilho/idem
    rev: v0.1.0
    hooks:
      - id: idem
```

It runs only when something that changes a render is staged, meaning `Chart.yaml`, any
`values*.yaml`, or anything under `templates/`. It then checks **every chart in the repository**,
because a `values.yaml` edit changes what every template in that chart renders.

You need Go (the hook builds `idem` itself) and `helm` on your `PATH`. Install it in repositories
that hold charts: `idem .` exits 2 where there is no chart to find, which would fail every
commit. To report without blocking one, override the default `--strict`:

```yaml
      - id: idem
        args: []
```
