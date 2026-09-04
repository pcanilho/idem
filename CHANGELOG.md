# Changelog

All notable changes to **idem** are documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Release notes are taken from this file: `.github/workflows/release.yml` extracts
the section matching the tag and hands it to goreleaser, so a tag with no
section here fails the release rather than publishing generated commit subjects.

## [0.1.1] - 2026-09-04

One bug fix, and the one behaviour change it implies. `idem` now weighs the
values you supply yourself against the ones it could not resolve, so the
"values idem cannot reach" caveat can be cleared by answering it.

### Fixed

- **The missing-values report ignored `-f` and `--set`** ([#4]). A release whose
  values come from an ApplicationSet generator idem cannot expand is reported as
  `could not be built: ... needs <keys>`, or, once it renders, as
  `rendered without values idem cannot reach ... missing <keys>`. Supplying
  those values on the command line let the chart build but left the report
  unchanged, so the caveat survived being answered - and a `--strict` gate over
  a generator-driven estate could never reach green however correct its charts
  were. What a `-f` file or a `--set` defines is now subtracted from what idem
  reports as unreachable, in text, `-o json` (`unconstructed` and
  `summary.unconstructed`), markdown and GitHub output alike.

  Three things decide what may be cleared, and each of them is a rule against
  clearing too much - a caveat wrongly dropped is idem silently reporting on a
  release nobody deploys, which is the failure it exists to catch:

  - **Only your own flags count.** A values file the repository names is not an
    assertion about what the generator supplies; a flag typed at the terminal
    is. This mirrors the precedence `-f` and `--set` already had over the
    delivery config when rendering.
  - **Only values keys can be cleared.** The unresolved list also names sources
    idem cannot reach - Flux's `valuesFrom Secret/x`, a multi-source
    `$values (…, from another source)`, a `valueFiles` path a generator
    templates - and no `-f` or `--set` answers one of those, so they are never
    credited.
  - **`-f` and `--set` are weighed in helm's order, not OR-ed together.** Helm
    coalesces a later map into an earlier one but lets a later scalar *replace*
    the subtree, and applies every `--set` after every `-f`. So
    `-f base.yaml --set webRoute=false` leaves `webRoute.enabled` nowhere to
    live, and `base.yaml` is no longer credited for it.

  Helm's `--set` syntax is read the way helm reads it, because a key read
  differently would credit you for a value helm was never given: `a\.b` is the
  single key `a.b` rather than a path into `a`, `a\,b` is one value carrying a
  comma rather than two assignments, `a.b.c` defines `a` and `a.b` while
  `a.b` does not define `a.b.c`, and `servers[0]` is not `servers[1]`.

### Changed

- **A chart that still fails once you have supplied everything is now exit 2.**
  It is reported as `could not be rendered` rather than `could not be built`,
  where before it was reported as unbuilt and exited 0. This is the point of the
  fix rather than a side effect of it: `could not be built` exists so idem never
  blames a chart for a value idem itself withheld, and once you have supplied
  them all, a remaining failure belongs to the chart or to the values. A run
  with no `-f` or `--set` is unaffected - the full 16-chart estate this was
  measured against is byte-identical to 0.1.0.

### Documentation

- `docs/limits.md` now says how to clear the caveat, next to the limit it
  qualifies.
- This file, and `.github/workflows/release.yml` now builds release notes from
  it. The generated changelog in `.goreleaser.yaml` is disabled rather than left
  in place, so there is one source of release notes and not two.

## [0.1.0] - 2026-08-25

Initial public release.

`idem` renders a Helm chart more than once, compares the results structurally,
and names the objects that will never settle - under ArgoCD, under Flux, or
under plain Helm - along with the `ignoreDifferences` or
`driftDetection.ignore` that stops the churn. The unit of analysis is a
**release**: chart plus values plus engine, never a chart alone.

It shells out to whichever `helm` is on your `PATH` rather than vendoring the
Helm SDK, which is what ArgoCD's repo-server does, and prints the version it
used on every run.

### Added

- `idem [chart]` checks one chart or a tree of them, `idem diff a.yaml b.yaml`
  compares two renders you produced yourself, and `idem doctor` asks a cluster
  you already run what keeps rolling.
- Per-engine verdicts for `argocd`, `flux` and `helm`, each with the fix block
  that engine can actually evaluate - and only where that engine churns.
- Reads your `Application`, `ApplicationSet` and `HelmRelease` for the values,
  release name, namespace and suppressions a release is deployed with.
  ApplicationSet generators whose input is the repository are expanded, one
  release per element.
- Findings are informative by default; `--strict` exits 1. Exit 2 means a chart
  could not be rendered, or idem itself failed, and is always fatal.
- Output as `text`, `json`, `yaml`, `markdown` or `github`, plus a GitHub Action
  and a pre-commit hook.
- `--new-from-rev` / `--new-from-merge-base` ratchet findings to what changed,
  with no baseline file.
- Signed build provenance: `gh attestation verify <archive> --repo pcanilho/idem`
  answers who built a binary and from which commit. `checksums.txt` is
  integrity, never authenticity.

### Dependencies

- Go 1.27
- `gopkg.in/yaml.v3` v3.0.1

[#4]: https://github.com/pcanilho/idem/issues/4
[0.1.1]: https://github.com/pcanilho/idem/releases/tag/v0.1.1
[0.1.0]: https://github.com/pcanilho/idem/releases/tag/v0.1.0
