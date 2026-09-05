# Changelog

All notable changes to **idem** are documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Release notes are taken from this file: `.github/workflows/release.yml` extracts
the section matching the tag and hands it to goreleaser, so a tag with no
section here fails the release rather than publishing generated commit subjects.

## [Unreleased]

## [0.2.0] - 2026-09-05

Three ways a run that checked nothing could read as clean, and the flag for the
one chart `idem` cannot do anything about.

### Added

- **`--exclude`, repeatable, to leave a chart out of a sweep** ([#9]). A chart
  helm cannot render is exit 2, always fatal and outside the ratchet, so one of
  them anywhere in a tree made the whole sweep permanently red with no way out
  and no flag to reach for. Some cannot be fixed from idem's side at all: a
  vendored `.tgz` over helm's 5 MiB chart-file limit, for one.

  This is input selection, not finding suppression. Nothing is rendered, so it
  is not a second place to configure idem, and the seam for suppressing
  findings is still `-o json | conftest`.

  A pattern with no `/` matches a chart directory at any depth, as
  `.gitignore` does; with one, it matches the path from wherever idem was
  pointed. Excluded charts are counted on the provenance line, following
  `type: library`, and a pattern that matched no chart is named, because a
  filter that addresses nothing reads as protection and gives none. Excluding
  every chart is an error rather than a clean run: "All 0 charts render
  consistently" is the worst sentence idem could print.

- **v0.1.1 published with empty release notes.** `.goreleaser.yaml` set
  `changelog: disable: true`, reasoning that a generated changelog would be a
  second source of notes competing with this file. But goreleaser reads
  `--release-notes` *inside* the changelog pipe, so disabling it stopped
  goreleaser reading the notes file as well as generating one, and the release
  body came out empty.

  Nothing failed. The extraction step wrote 62 correct lines, goreleaser exited
  0, the attestation ran, and every check was green: a release body is the one
  artifact nothing else looks at. Confirmed by running goreleaser both ways -
  "generating changelog" appears in the log only when the pipe is enabled.

  The pipe is enabled again. `--release-notes` still wins over anything it would
  generate, so there was never a second source to remove. A test now fails if
  the pipe is disabled. The published v0.1.1 notes were filled in afterwards;
  immutable releases lock the tag and the assets, not the body.

### Fixed

- **`--strict` exited 0 when a release could not be built** ([#5]). A release
  whose values come from a generator idem cannot expand is reported as
  `could not be built`, and that count reached no exit code at all, so
  `idem <chart> --strict` printed the gap and passed. A chart nobody checked was
  indistinguishable from a chart that came back clean, which is the one outcome
  a strict mode exists to prevent.

  It is exit 1 now, never exit 2: idem withheld the values, so the chart is not
  at fault, and a run without `--strict` is unchanged. Clear it the way
  `docs/limits.md` already says, with `-f` or `--set`.

  The gate is scoped to the ratchet, unlike `could not be rendered`. A chart
  that will not render is a coverage gap no branch closes, so it escapes
  `--new-from-rev` and stays fatal. This is idem's own limit, and asking a
  branch to name every generator value in an estate it never touched would shut
  the on-ramp in `docs/ci.md` on the estates that need it most.

  The other half of the report was already correct: `could not be rendered` has
  always exited 2, with or without `--strict`.

- **`-o markdown` wrote nothing for a release it could not build**, so the
  documented pull-request recipe posted no comment. Paired with the exit code
  above that is a red build with no reason anywhere on the pull request. It now
  writes the same collapsed block the other outcomes get, ratchet-scoped to
  match the gate. `-o github` says it as a warning rather than a notice, which
  is the quietest thing GitHub renders. The 0.1.1 notes claimed markdown already
  reported this; it did not.

- **A chart with a template defect was excused as "could not be built"** ([#8]).
  `unbuilt()` was a disjunction of two unrelated facts, the render failed and
  something is unresolved, and never asked whether one caused the other. So any
  chart standing near a manifest idem cannot expand escaped `could not be
  rendered`, and idem printed "The chart is not at fault" about a chart that
  was. That put a genuinely broken chart outside exit 2, the one code nothing
  may ignore.

  The render error now has to be one a value could have caused, verified
  against helm 3.9.4, 3.14.4, 3.19.0 and 4.2.4:

  - **A parse error is refused, and that is structural rather than a guess.**
    `pkg/engine` parses every template from its raw bytes in a loop that
    finishes before the first `ExecuteTemplate`, so no value exists yet when a
    parse error is produced. The wording is helm's own and byte-identical
    across all four versions.
  - **`execution error at (` is not the positive test.** helm emits it only
    through `warnWrap`, from the two `required` branches and `fail`. A nil
    pointer or a wrong type carries no such marker and is exactly what a
    withheld value produces, so testing for it would reject the cases this
    exists to accept.
  - **Everything else must name a template.** A missing or malformed
    `Chart.yaml`, an unresolved dependency, helm's 5 MiB chart-file limit and a
    `type: library` chart all fail in the loader and name no template, so no
    value could have changed the outcome. A `values.schema.json` violation is
    the one value-caused failure with no template location, so it is matched on
    its own.

- **The documented pull-request comment fired on every run** ([#10]).
  `hashFiles` hashes any file that matches and never looks at its size: the
  runner's `hashFiles.ts` sets `hasMatch` on the file count, and its only
  `statSync` tests `isDirectory()`. The action's redirect creates the report
  before idem writes a byte, so a clean run left a zero-byte file whose digest
  is a perfectly good non-empty string, and
  `if: hashFiles('idem.md') != ''` could never tell "nothing to say" from
  "something to say". The action removes an empty report, which is what makes
  the documented recipe mean what it says.

### Changed

- The release workflow installs cosign before goreleaser. `goreleaser-action`
  verifies the goreleaser binary it downloads only when cosign is on `PATH`;
  without it the run logged "cosign not found in PATH, skipping signature
  verification" and carried on, which is a supply-chain check that silently did
  nothing. Third-party actions in the release workflow are now held to a commit
  SHA by test, as `action.yml`'s already were.

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
  it: the section matching the tag is extracted and passed to goreleaser with
  `--release-notes`.

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
[#5]: https://github.com/pcanilho/idem/issues/5
[#8]: https://github.com/pcanilho/idem/issues/8
[#9]: https://github.com/pcanilho/idem/issues/9
[#10]: https://github.com/pcanilho/idem/issues/10
[Unreleased]: https://github.com/pcanilho/idem/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/pcanilho/idem/releases/tag/v0.2.0
[0.1.1]: https://github.com/pcanilho/idem/releases/tag/v0.1.1
[0.1.0]: https://github.com/pcanilho/idem/releases/tag/v0.1.0
