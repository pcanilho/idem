# idem — design notes

Why `idem` works the way it does. The [README](../README.md) covers what it is and how to
use it; this covers the reasoning, the limits, and the evidence.

---

## 1. What `idem` is actually judging

**A release, not a chart.** The same chart is stable with one set of values and churns with
another — set `auth.existingSecret` and the random branch never fires; leave it unset and it
fires every render. A verdict is a property of **chart + values + engine**, and a tool that
quietly rendered defaults would be answering a question nobody asked.

So `idem` renders with *your* values, and when it detects an `Application` or a `HelmRelease`
it reads the values wiring out of it rather than starting from chart defaults.

### The completeness rule

Values can live somewhere `idem` cannot see. A Flux `HelmRelease` may defer them to
`valuesFrom` ConfigMap/Secret references, or to `${VAR}` substitution performed by the
Kustomization's `postBuild` — both resolve **in the cluster**, at reconcile time. An ArgoCD
multi-source Application can pull `$values` from a second repository.

When that happens, `idem` says so instead of guessing:

```console
  postgresql/templates/secrets.yaml
    Secret/pg-postgresql   .data.postgres-password

      argocd    CHURNS     every sync
      flux      unknown    values incomplete

      2 of 3 value sources resolved. `valuesFrom` → Secret/pg-values could not be
      read without --context=, and it may set auth.existingSecret. Re-run with
      --context for a definitive answer.
```

**`idem` never silently renders defaults.** If a value source cannot be resolved, the verdict
is qualified and the missing source is named — the same discipline applied to `lookup`: say
`unknown`, say why, say what would resolve it.

### Why this makes `--context` near-mandatory for Flux

The reverse of what you would expect. An ArgoCD `Application` carries
`spec.source.helm.{values,valueFiles,parameters}` inline, so a git checkout is usually enough
to reconstruct the effective values. A Flux `HelmRelease` frequently does not — `valuesFrom`
and `postBuild.substituteFrom` exist precisely so per-stage configuration lives outside the
manifest.

For Flux, `--context` is often the only way to get a truthful answer at all.

### Where `idem` stops

It renders a chart with values. It does not reconstruct your delivery pipeline:

- **Kustomize overlays that patch a `HelmRelease`** are not applied. Point `idem` at the built
  output, or pass the resulting values directly.
- **`postBuild` variable substitution** is not performed.
- **Post-renderers run after Helm**, so a `spec.postRenderers[].kustomize` patch can introduce
  a difference Helm never produced — or mask one it did. `idem` does not run them, and a
  finding is attributed to the render it observed, not to a patch applied afterwards.

Reproducing all of that faithfully means reimplementing two controllers, at which point the
answer is worth less than running the controllers themselves.

---

## 2. How each verdict is reached

| Verdict | How | Certainty |
|---|---|---|
| `argocd: CHURNS` | rendered twice with `helm template`, output differed | **observed fact.** `helm template` resolves `lookup` to `{}` by construction, so this *is* the ArgoCD condition |
| `flux/helm: CHURNS` | the chart contains no `lookup` at all | **sound.** Nothing exists that could stabilise the value |
| `flux/helm: stable` | with `--context`, rendered twice via `--dry-run=server`, output identical | **observed fact.** No inference — this is how those engines actually render |
| `flux/helm: unknown` | the chart contains `lookup`, but not provably guarding *this* value | **honest.** Establishing the link means evaluating the template through `include` into subchart helpers |

`idem` does not evaluate templates. Bitnami's idiom lives in
`charts/common/templates/_secrets.tpl`, reached from `templates/secrets.yaml` via
`_helpers.tpl` — a tracer would return `unknown` on the flagship chart anyway, so claiming
more would buy nothing but a wrong answer.

### What `idem` actually runs

Everything goes through `helm`. `idem` never invokes the `flux` CLI or the Flux SDK.

| Engine | What `idem` runs | Fidelity |
|---|---|---|
| `argocd` | `helm template` (`--dry-run=client`) | **exact** — ArgoCD shells out to the `helm` binary and does the same |
| `flux` | `helm template --dry-run=server` | **sound for `lookup`, not a full reproduction** |
| `helm` | same | same |

helm-controller performs a real install/upgrade *action* through the SDK, so
`.Release.IsUpgrade` is true, `.Release.Revision` increments, hooks run, and Flux truncates
release names to 53 characters with a hash suffix. `--dry-run=server` gets none of that. It
gets the **`lookup` semantics** right, which is the one thing this check turns on.

This is also why `flux` is an `Engine` and not a `Renderer` in the code: claiming to render
Flux would be an overclaim, so the type system does not permit it.

---

## 3. `--context`

`helm template` defaults to `--dry-run=client` and resolves `lookup` to `{}` by construction —
exactly what ArgoCD's repo-server does. `--dry-run=server` resolves it for real, which is what
Flux and `helm upgrade` do. Verified:

```console
$ helm template p ./chart                    # lookupResolved: "NO"
$ helm template p ./chart --dry-run=server   # lookupResolved: "YES"
```

So rendering twice each way answers the question by observation rather than argument.

**`idem` never writes to your cluster.** `--dry-run=server` is a render-time query, not an
apply. No create, no update, no server-side apply, no ownership of anything. It does not manage
releases and will not learn to — deployment belongs to your GitOps engine.

Three caveats:

- **It needs read access to whatever the chart looks up.** A chart that looks up Secrets needs
  Secret read permission, which is privileged. Use a scoped context, not cluster-admin.
- **Results become environment-dependent.** The same chart can be `stable` against one cluster
  and `CHURNS` against another, because the answer genuinely depends on what is already there.
  That is the truth, not a defect — but it is why `--context` is opt-in and never inferred from
  a kubeconfig lying around.
- **Naming the looked-up object is best-effort.** `lookup "v1" "Secret" .Release.Namespace $name`
  has arguments that are themselves template expressions; `idem` names the object when they are
  literals and says so generically when they are not.

With `--context`, charts gated on `.Capabilities.APIVersions.Has` render against your cluster's
real capabilities rather than Helm's built-in defaults — but by a different route than you might
expect. `idem` does **not** pass `--api-versions`/`--kube-version`; `internal/helm` deliberately
omits them, because `--dry-run=server` means the API server has already supplied the real ones.
ArgoCD passes those flags precisely *because* its repo-server never does a server dry run, so the
two arrive at the same place by opposite means.

---

## 4. Render-side churn is an ArgoCD problem, and it is worth being precise

Scoped deliberately: **render-side** churn. Flux is exposed to the other two causes in
[§8](#8-five-causes-in-pipeline-order) just as much, and is quieter about it. What
follows is only about the first.

For render-side churn Flux is **not** affected the same way, and you should not believe anyone
who tells you otherwise — including me, without the receipts:

- Flux's helm-controller performs a **real Helm install/upgrade with cluster access**, so
  `lookup` resolves exactly as the chart author intended.
- It does not re-render on reconcile at all. Stefan Prodan, in
  [helm-controller#1119](https://github.com/fluxcd/helm-controller/issues/1119): *"Drift
  detection in Flux means comparing the Helm storage against the cluster, we never render the
  templates."*
- `driftDetection.mode` defaults to `disabled`, so there is no `selfHeal` equivalent by
  default. Note this cuts both ways: it is why Flux does not churn, and also why Flux would not
  tell you if it did.

ArgoCD works differently, on purpose. Jesse Suen, ArgoCD co-creator, in
[argo-cd#8551](https://github.com/argoproj/argo-cd/issues/8551):

> *Fundamentally, we work differently than helm because we invoke `helm template` early and
> often. Whereas helm only invokes this at point of install or upgrade.*

Cluster access for repo-server is a long-standing request —
[argo-cd#5202](https://github.com/argoproj/argo-cd/issues/5202), open since January 2021 — and
architecturally unlikely to change.

Helm upstream was asked for a deterministic render mode in
[helm#10689](https://github.com/helm/helm/issues/10689). It received exactly one comment — from
a stale bot — and was auto-closed. No maintainer ever replied.

**A caution about comparing engines yourself.** It is tempting to diff `argocd app manifests`
against `flux build helmrelease` and call the result "how ArgoCD and Flux differ". That is not
sound: helm-controller performs a real install through the SDK against a live cluster, and
`flux build` is a local approximation of it, not a reproduction. Neither is `helm template`.
This is why `idem` reasons about the difference rather than claiming to reproduce it.

---

## 5. Two kinds of finding

`idem` runs two independent checks and never mixes their output.
("Engine" always means a GitOps engine — ArgoCD, Flux, Helm.)

**Observed** — the chart was rendered twice and the output differed. A fact.

**Potential** — the chart contains a function that cannot render consistently, but a pinned
value is currently suppressing it. A warning. Always shown, always in its own section, never
counted in the headline number, never affecting the exit code:

```console
  — potential · not counted, not fatal —
  lab/templates/registry.yaml:22   genSelfSignedCert   present, did not fire this render
```

A static warning is sometimes wrong — a pin may be perfectly sound — and a tool that cries wolf
about the potential case teaches you to distrust it about the observed one. But the failure
that cost me two years was a pin that silently stopped applying, so hiding these would hide the
thing most worth knowing.

The non-deterministic function set is audited against sprig v3.3.0, which both Helm 3.19 and
Helm 4.2 pin, so one list covers both lines:

`randAlphaNum`, `randAlpha`, `randNumeric`, `randAscii`, `randBytes`, `randInt`, `shuffle`,
`uuidv4`, `bcrypt`, `htpasswd`, `genPrivateKey`, `genCA`, `genCAWithKey`, `genSelfSignedCert`,
`genSelfSignedCertWithKey`, `genSignedCert`, `genSignedCertWithKey`, `encryptAES`, `now`,
`ago`, `getHostByName`, `keys`, `values` — plus `lookup`.

`keys` and `values` were missed by the first audit and are arguably the worst of the set. They
build a slice from Go's map iteration order and never sort it, so they reorder on **every**
render — and unlike a fresh UUID the result looks plausible in a diff, so a human reviewing the
output waves it through. `sortAlpha` is the fix. Because both are ordinary English words, they
are only reported where they read as an actual call: `.Values.keys` and a YAML field named
`keys:` are not warnings.

`env` and `expandenv` are deliberately absent: Helm **deletes** them from the function map, so a
chart calling one fails to parse. `hostname` is absent because sprig v3.3.0 has no such function.
`getHostByName` is included even though Helm stubs it to `""` without `--enable-dns`, since a
chart cannot control the flag its consumer passes.

Deterministic despite appearances, and deliberately **not** flagged: `derivePassword`,
`buildCustomCert`, `decryptAES`.

---

## 6. Why shell out to `helm`

`idem` does not vendor Helm. It runs the `helm` on your `PATH`.

- **Fidelity comes from your toolchain, not mine.** ArgoCD does the same — its repo-server
  calls `exec.CommandContext(ctx, "helm", ...)` and does not import the Helm SDK at all.
  Reproducing what ArgoCD does means doing what ArgoCD does.
- **It lets you compare renderers.** `--helm /path/to/helm3` against `--helm /path/to/helm4` is
  two flags. A vendored SDK can only ever render one version.
- **Registry authentication is helm's, not mine.** A vendored SDK would mean reimplementing
  registry auth for every cloud provider, and getting credential-helper handling wrong is a
  good way to leak a token.

That second point is not hypothetical. **ArgoCD 3.5 ships Helm 4 only** — `helm.version: v3` is
accepted and silently ignored — so upgrading 3.4 → 3.5 swaps your renderer from Helm 3.19 to
4.2 underneath you. Helm 4 changed `.Capabilities.KubeVersion`'s default from a hardcoded
`v1.20.0` to a runtime-derived value (`v1.36.0` on 4.2.4), so any chart doing
`semverCompare ">=1.21-0" .Capabilities.KubeVersion.Version` renders differently with no Git
change at all.

---

## 7. `idem diff`, and why `check` cannot read stdin

The comparison engine is the primitive, and it is exposed directly: two rendered manifest sets,
compared structurally, GVK-aware and order-insensitive. No Helm, no network, no cluster.

Detecting non-determinism means rendering **more than once**, so `idem check` has to invoke the
renderer itself — a single stream cannot show you a difference. That is why `idem` cannot take
a pipe the way `kubeconform` does, and why `idem diff` exists for when you would rather produce
the renderings yourself.

It also makes kustomize a target for free: `kustomize build a/ > a.yaml`,
`kustomize build b/ > b.yaml`, `idem diff a.yaml b.yaml`. Pure kustomize is deterministic, so
the churn bug does not arise there — but kustomize *inflating a Helm chart* inherits it, and
kustomize's own version drift is real
([kustomize#6058](https://github.com/kubernetes-sigs/kustomize/issues/6058), 38 👍, open:
5.8.0 silently changed namespace handling for helm-inflated charts).

---

## 8. Five causes, in pipeline order

Everything that puts something in your cluster that git does not describe happens at one of
five stages. They run in order, each needs different evidence, and no single mechanism finds
them all.

| | Cause | Where it happens | How to detect |
|---|---|---|---|
| **0. Source-side** | what "the chart" resolves to is not pinned | `Chart.yaml` dependencies | compare the constraint against what is deployed — **analysed, not built** |
| **1. Render-side** | the chart renders differently each time | `helm template` | render twice, compare — offline |
| **2. Engine-side** | the GitOps engine adds, strips or rewrites fields after rendering | ArgoCD / Flux | compare against the engine's config — offline |
| **3. Admission-side** | a webhook or the API server mutates the object as it is applied | admission chain | `--dry-run=server`, compare to what was sent |
| **4. Post-apply writes** | another controller writes into the object *later* | ESO, cert-manager, operators | compare live against `last-applied` — **retrospective only** |

### Why render-side gets a tool of its own

Worth stating, because it is the obvious objection: **render-side churn is a minority of real
`OutOfSync` reports.** Admission and defaulting cause far more of them, and ArgoCD's Server-Side
Diff addresses that dominant class natively.

Two things answer it. Render-side is the only cause on this list detectable **statically,
offline, before merge** — every other row needs a running cluster, and cause 4 needs the drift to
have already happened. And nothing else generates the suppression config: `argo-cd#5453` has been
open since February 2021.

`idem` covers causes 2, 3 and 4 as well, and says so where those features live rather than in a
taxonomy at the front of the README. This argument lives here rather than there for the reason
§13 gives: a README that argues with an objection the reader has not raised is an author arguing
with a stranger.

Causes 3 and 4 look similar and are not. Admission mutation is **synchronous**: it happens
during the apply, so a server dry-run reproduces it. A post-apply write is **asynchronous**,
arriving seconds or minutes afterwards, so a dry-run cannot see it at all — the object comes
back clean and the drift appears later.

### 0. Source-side — git does not say which chart

A `Chart.yaml` dependency may declare a range rather than a version:

```yaml
dependencies:
  - name: romm
    version: "=>9.2.9"
```

Rendering resolves that against the repository index *at render time*. Two renders a second
apart agree; two renders a month apart need not. Nothing in git records which version was
actually used, so the repository does not describe what is running.

The deployed version is recoverable, because Helm stamps it on every object it renders:

```
  helm.sh/chart: flaresolverr-17.8.0
```

So the check would be a metadata comparison — no rendering, no cluster mutation, one label read.

**It is not built.** `idem` reads `helm.sh/chart` only to work out which release owns a workload
in `doctor`; nothing anywhere compares a declared constraint against a running version. This
section is analysis, and the numbers below were measured by hand rather than produced by the
tool. Said plainly because a design document listing five causes reads as a claim to cover five,
and four is the true number.

Measured on the author's `charts/home`, all 19 resolvable dependencies were running above their
declared floor:

```
  romm        =>9.2.9     running 19.4.0     (ten major versions above the floor)
  lidarr      =>23.0.2    running 29.7.3
  jackett     =>22.0.1    running 27.7.25
```

**This is not automatically a defect.** Floating ranges are how you get auto-update, and the
author uses them deliberately — the lock files are gitignored precisely so resolution is not
frozen. The finding is informational: *git does not describe what is running, and a rebuild or
a repo-server cache expiry will resolve again to whatever is newest then*. That is a feature if
you wanted auto-update and a surprise if you wanted reproducibility, and only the reader knows
which.

It becomes a churn cause when the re-resolution happens on its own: ArgoCD re-rendering after a
cache expiry can deploy a new subchart version with no git change at all — a change appearing in
the cluster with no corresponding change in the repository, which is the same family as
everything else in this list.

### 1. Render-side

The chart itself is unstable. Render twice, compare. This is the bulk of the tool and needs
nothing but `helm`.

### Which engines are exposed to which cause

Worth stating carefully, because "Flux is fine" is true of exactly one row:

| Cause | ArgoCD | Flux |
|---|---|---|
| **Render-side** | exposed | **genuinely immune** — `lookup` resolves and it does not re-render on reconcile |
| **Engine-side** | exposed | **exposed** — `postRenderers`, its own field management |
| **Apply-side** | exposed | **equally exposed** — the cluster mutates regardless of who applied |

The difference on the last two rows is **visibility, not immunity**. ArgoCD reports `OutOfSync`
continuously and loudly. Flux has no equivalent surface: a `HelmRelease` reports `Ready: True`
once the release succeeded, and `driftDetection.mode` **defaults to `disabled`**, so by default
it never compares against the cluster at all.

**Flux is quieter, not safer.** That distinction matters more than it sounds. The worst incident
behind this tool was not the Harbor pods rolling every sync — that was noisy and eventually
noticed. It was the Postgres password drifting from the database on a StatefulSet with no
checksum annotation, where nothing restarted and nothing alerted, for two years. Silence is a
failure mode, not a clean bill of health, and a Flux user who reads "not affected" and stops
looking has been given false comfort.

### 2. Engine-side — what the engine does to Helm's output

`helm template` output is **not** what ArgoCD stores as desired state. ArgoCD decorates it, and
the decoration can collide with what the chart already set.

**The classic is the tracking label.** ArgoCD's `application.instanceLabelKey` defaults to
`app.kubernetes.io/instance` — which is also a standard Helm label that most charts set to the
release name. When the Application name and the Helm release name differ, the two write
different values to the same key and the app is OutOfSync forever.

Measured on the author's cluster, where the collision is *avoided*:

```
  application.instanceLabelKey = argocd.argoproj.io/instance   (the recommended override)
  chart rendered               app.kubernetes.io/instance = lab       (release name)
  Application is named         lab-app                                 (per tracking-id)
```

Under ArgoCD's default configuration those last two would fight — Helm writing `lab`, ArgoCD
writing `lab-app`, on the same label, on every sync. The fix is a one-line ConfigMap change
that most operators have never heard of, which is exactly the kind of thing worth reporting.

Other engine-side transformations in the same family:

- **Namespace injection** — ArgoCD sets `metadata.namespace` from `spec.destination.namespace`
  on objects the chart rendered without one.
- **`releaseName` divergence** — `spec.source.helm.releaseName` differing from the Application
  name changes every label derived from `.Release.Name`.
- **Kustomize `commonLabels` / `namePrefix`** applied by an ArgoCD kustomize source, after
  Helm has rendered.
- **Flux `postRenderers`** — kustomize patches applied after helm-controller renders, which can
  equally introduce or mask a difference.

This class is detectable without a cluster *if* `idem` can read the engine's configuration. It
reads the `Application`/`ApplicationSet`/`HelmRelease` in your repository, which is where
`ignoreDifferences`, `driftDetection.ignore` and the sync options live. It does **not** read
`argocd-cm`, with or without `--context`, so the instance-label collision below is analysis
rather than a check idem performs.

### 3. Apply-side — what the cluster does to what was applied

No new machinery: render, ask the API server what it would actually store
(`kubectl apply --dry-run=server` — nothing is persisted), and feed both into the same
`Compare`. A Service submitted with two fields comes back with nine:

```
  .spec.clusterIP  .spec.clusterIPs  .spec.internalTrafficPolicy  .spec.ipFamilies
  .spec.ipFamilyPolicy  .spec.ports[0].protocol  .spec.ports[0].targetPort
  .spec.sessionAffinity  .spec.type
```

**Two caveats that keep this honest:**

1. **ArgoCD already normalises most plain defaulting**, so a raw diff of rendered-vs-stored
   overstates the problem. What actually breaks people is a mutating webhook writing fields
   ArgoCD's normaliser does not expect. `idem` reports the diff and says that much in one
   sentence; it does **not** read `admissionregistration.k8s.io`, so it cannot name which
   webhook did it. Naming them would need the rules matched against each object, and that is
   not built.
2. **Pod-level injectors do not cause Deployment-level drift.** The Vault agent injector and
   Istio's sidecar injector match `pods`; ArgoCD compares Deployments. The mutation happens when
   the Pod is created from the template, not when the Deployment is applied, so it never appears
   in a Deployment diff. Claiming otherwise would be the kind of plausible-sounding error this
   document exists to avoid.

The precise version of this analysis is server-side-apply field ownership: the response's
`managedFields` names which manager owns which field, so a field owned by someone other than you
is one you are being fought for. That is the sound long-term mechanism; the dry-run diff is the
cheap approximation.

### 4. Post-apply writes — what another controller does afterwards

A dry-run cannot find these, because the write has not happened yet. But the object records
what was sent to it, so the drift can be read straight off the live object with no chart, no
rendering and no dry-run:

```
diff( metadata.annotations["kubectl.kubernetes.io/last-applied-configuration"], live )
```

Measured on the author's cluster, where ArgoCD reported `Secret/lab-gitea-mirror` as OutOfSync
and `argocd app diff` refuses to say why:

```
  keys only in live : BETTER_AUTH_SECRET, ENCRYPTION_SECRET, GITEA_TOKEN, GITHUB_TOKEN
  label only in live: reconcile.external-secrets.io/managed
```

ArgoCD applied a Secret with no data; External Secrets Operator populated it afterwards.
Permanent divergence, correctly diagnosed, from one API read. Twelve Secrets on that cluster
show the same shape.

This is the common pattern wherever a controller is *designed* to write into an object someone
else declares — ESO populating Secrets, cert-manager writing `tls.crt`, operators filling in
status-like spec fields. It is not a bug in either party; it is two owners for one object, and
the remedy is the same `ignoreDifferences` as everywhere else.

**Two apply modes, two mechanisms.** `last-applied-configuration` exists only under client-side
apply. Under server-side apply there is no such annotation, and the equivalent information is in
`managedFields` — which names the owning manager per field, and is strictly better. `idem` reads
whichever is present. (On the author's k3s 1.26 cluster the Secret carried a
`last-applied-configuration` and **zero** `managedFields` entries, so supporting only the
modern route would have found nothing.)

### `idem doctor`

Retrospective rather than predictive: which workloads are *already* churning, whatever the
cause. Two queries and a ranking — `deployment.kubernetes.io/revision` normalised by age,
cross-referenced with `checksum/*` annotations on the pod template.

It is explicitly a heuristic. Revision count also rises from deploying often, so the signal is
the *combination*: far above the cluster's median rollout rate, and carrying a checksum
annotation derived from a Secret. `doctor` ranks suspects; `idem <chart> --context=` establishes
cause. Measured on the author's cluster, the top five by rate were the two charts already known
to be non-deterministic plus one that was not.

### Other vectors in the same families

Catalogued from ArgoCD's own [diffing documentation](https://argo-cd.readthedocs.io/en/stable/user-guide/diffing/),
which is the closest thing to an authoritative list. Each belongs to one of the four causes:

| Vector | Cause | Note |
|---|---|---|
| **HPA writes `spec.replicas`** | 4 | The most famous instance. Argo's own remedy is `jsonPointers: [/spec/replicas]` |
| **HPA reorders `spec.metrics`** | 4 | A *controller reordering a list*. Argo's advice is to order it in Git the way the controller prefers ([k8s#74099](https://github.com/kubernetes/kubernetes/issues/74099)) |
| **Aggregated ClusterRoles** | 4 | The aggregation controller rewrites `rules`. Argo has `ignoreAggregatedRoles` for it |
| **Unknown or misspelled fields** | 3 | A field not in the schema is dropped on apply, so live never matches Git. Statically detectable, and usually a typo worth reporting as a bug rather than ignoring |
| **`status` rendered by the chart** | 3 | Committed status is stripped. Purely a chart defect and detectable offline |
| **Sops / SealedSecrets decryption** | 4 | Same shape as ESO: a controller populates what was applied empty |

---

## 9. Where `idem` itself can be wrong

Every tool that reports on correctness owes the reader a list of the ways it is wrong. Three are
structural rather than incidental.

### Quantity and IntOrString normalisation — a false-positive source

The API server canonicalises certain typed values. Measured:

```
  sent  cpu: 0.1          ->  server stores  "100m"
  sent  memory: 1073741824 -> server stores  1073741824   (unchanged)
```

`0.1` and `100m` are the *same quantity*. A purely structural comparison calls that a
difference, so the apply-side check would report drift on any chart expressing CPU as a
decimal. The same applies to `IntOrString` fields such as `targetPort`, and to any CRD field
declared with a Kubernetes type.

**This means structural comparison alone is not sufficient for the apply-side check.** Known
typed fields need semantic comparison — parse both sides as a `Quantity` and compare values,
not strings. ArgoCD hit exactly this and solved it with `resource.customizations.knownTypeFields`,
which is worth reading before reinventing it. Until that exists, apply-side findings on
`resources.*` and port fields should be suppressed rather than reported wrongly.

Note this affects **only** the apply-side and post-apply checks, where rendered output is
compared against cluster state. The render-side check compares two `helm template` runs, which
both produce the same textual form, so it is unaffected.

### The evidence itself can be mutated

Several checks read facts off live objects and trust them:

| Check | Evidence it trusts |
|---|---|
| Cause 0 — floating dependencies | the `helm.sh/chart` label |
| Cause 4 — post-apply writes | the `last-applied-configuration` annotation, or `managedFields` |
| `doctor` ranking | `deployment.kubernetes.io/revision`, `checksum/*` annotations |

All of those are ordinary labels and annotations, and **a mutating admission controller can
rewrite any of them**. Kyverno `mutate` policies are the common case — a broad "stamp these
labels on everything" policy will happily clobber `helm.sh/chart`, and a policy that adds
labels can manufacture evidence that was never there. This is mechanically just
[cause 3](#3-apply-side--what-the-cluster-does-to-what-was-applied), but the consequence is
different in kind: it does not create drift, it corrupts the reading.

There is no way to detect this from the object alone — a mutated label looks exactly like an
honest one. What `idem` *could* do is say how much to trust the reading, by enumerating what is
in the mutation chain. **This is a sketch of an unbuilt check, not output `idem` produces** — it
reads no `admissionregistration.k8s.io` and no Kyverno policies today:

```text
  SKETCH, NOT IMPLEMENTED
  note: 2 mutating webhooks and 1 Kyverno mutate policy can rewrite labels on
        these objects. Evidence read from live state is only as reliable as
        those allow.
          common-vault-agent-injector-cfg     pods
          cnpg-mutating-webhook-configuration clusters, backups
          ClusterPolicy/istio-auto-inject     namespaces  (mutate: labels)
```

That would be honest without being paranoid: on a cluster whose only mutators touch `pods` and CNPG
resources, a `helm.sh/chart` label on a Deployment is entirely trustworthy, and saying so is
more useful than a blanket disclaimer. The rule is the same as everywhere else in this
document — state the provenance, and let the reader judge.

### Temporal non-determinism — a blind spot in the method

`idem` renders twice, back to back. That finds anything random or clock-driven. It cannot find
anything that resolves consistently *today* and differently *later*:

- **Unpinned chart dependencies** — `dependencies: [{version: "^1.0"}]` resolves to whatever is
  newest when the repository index is refreshed. Two renders one second apart agree; two renders
  a month apart do not.
- **Floating image tags** — `:latest`, `:stable`. The rendered manifest is byte-identical every
  time; what it points at is not.
- **Repository index drift** — the same `--version` constraint against a re-published chart.

These produce real, surprising drift, and no amount of re-rendering in one session will show
them.

**Unpinned dependencies are the one case with a cheap answer**, and it is
[cause 0](#0-source-side--git-does-not-say-which-chart): compare the declared constraint against
the version stamped on the deployed objects. No rendering required. Floating image tags and
re-published chart versions have no such shortcut — they need a render compared against a
*recorded* earlier render, which is the `idem diff` primitive with one side stored.

A clean `idem` run means **"this renders consistently right now, on this machine, with this
helm"**. It does not mean the chart is pinned, reproducible, or safe next month.

---

## 10. Decisions borrowed rather than invented

**The ratchet is golangci-lint's.** `--new-from-rev` / `--new-from-merge-base`, and
deliberately not a baseline file. The reasoning is from golangci-lint's own help text — precisely,
from the help for `--new`/`-n` rather than for `--new-from-rev`, whose own help is the one-liner
*"Show only new issues created after git revision REV"*: *"It's not practical to fix all existing
issues at the moment of integration: much better to not allow issues in new code."* The same block
ends with the line that argues hardest for this shape — *"For CI setups, prefer
`--new-from-rev=HEAD~`"*. Git already knows what changed; storing a second copy of that knowledge in a checked-in
allowlist is a format to maintain and a thing to review, for no gain. The granularity differs —
golangci-lint scopes to changed lines, `idem` scopes to changed charts, because a finding
belongs to a rendered object rather than a source line.

**Parallelism is per-chart.** Charts are independent, so rendering is embarrassingly parallel;
the default is `NumCPU` with `--jobs` to override. Two consequences to keep in mind: helm's
stderr must be buffered per chart or interleaved output becomes unreadable, and a large repo of
OCI-referenced charts can produce a pull storm, which is one reason `--jobs` exists.

**List elements are matched by name where Kubernetes has one.** Containers, env, ports, volumes
and volumeMounts are name-keyed by convention. Matching them positionally means an element
inserted at index 0 reports every later index as changed — one edit becoming an avalanche of
false paths. Everything else falls back to positional matching.

---

## 11. Output

Three renderings of one `Report` struct, and the split is deliberate.

**`text`** is a status line that occasionally expands, not a report. The modal run finds
nothing and prints two lines; a run with findings prints one line per finding and one
remediation block. No box drawing and no emoji severity badges — not on aesthetic grounds, but
because emoji have inconsistent width across terminals and would break the column alignment on
exactly the terminals that cannot be tested, and because box-drawing characters make `grep`,
`awk` and output diffing worse on a tool people will pipe.

**The last line is a verdict, not a stat line.** `2 of 10 charts will churn under ArgoCD; 1
could not be rendered` beats `10 charts · 7 consistent · 2 differ · 1 unevaluable`, which makes
the reader do arithmetic to find out whether to care. Borrowed from
[iam-policy-validator](https://github.com/boogy/iam-policy-validator) — from its `--format
enhanced`, to be exact. Its *default* `console` format falls back to the stat fragments
(`✗ 2 AWS-invalid`, `⚠ 3 with findings`) that this rule moved away from, so the citation is for
the shape one of its formats takes, not for the tool's default behaviour.

**`json`** is the machine contract and the documented substitute for the rules system that was
cut, so its shape is stable from the first release: enums marshal as names rather than
ordinals, and each path carries both its dotted and RFC 6901 renderings so no consumer has to
reimplement the escaping.

**`markdown`** exists for one job — a pull-request comment. A table survives GitHub's renderer
without alignment tricks, and the remediation goes in a `<details>` block because it is long
and only some readers need it. `gh pr comment --body-file -` is then the entire CI integration.

**No HTML, CSV or SARIF.** JSON covers every machine consumer and markdown covers the human one
that matters; each further format is a rendering to maintain forever, and SARIF in particular
buys GitHub code-scanning integration for a tool whose findings are not code defects.

---

## 12. Implementation notes

**Paths are segments, not strings.** A Kubernetes key may contain `.` or `/` —
`.data.application.yaml` and `.metadata.annotations.checksum/secrets` are both common — so a
flat string cannot say where one segment ends and the next begins. The checksum annotation in
particular needs RFC 6901 escaping (`checksum/secrets` → `checksum~1secrets`) to appear in an
`ignoreDifferences` block, and `~` must be escaped before `/` or the escape itself is
re-escaped.

**Absent is not null.** A key missing on one side and a key present with a null value are
different facts; conflating them lets a generated `ignoreDifferences` entry target a field that
does not exist.

**Duplicate identities are an error, not a last-write-wins.** Silently dropping an object
before comparison, in a tool whose claim is "the output is stable", is a soundness hole.

**Source provenance is free but file-level only.** `helm template` marks each document with a
`# Source:` comment naming the template that produced it. There are no line numbers in render
output — those come from the separate lexical scan of the chart's templates. `argocd app
manifests` output has been through the repo-server's decoding and has lost the comments
entirely, so findings from that path group under `(source unknown)`.

---

## 13. Decisions the README used to argue for

These were in `README.md`, where they read as pre-emptive defences against feature requests from
users who did not exist yet — the effect on a stranger being "this author will argue with me".
They are arguments about design, so they belong here.

### No rules file, and no exceptions file

Suppression is something you need *after* you have run a tool and disagreed with it; nobody has
exceptions on day one. Shipping the mechanism first invites a config file that outlives the
reason for every line in it.

`-o json` is the seam instead, and OPA is the policy engine:

```sh
idem ./charts -o json | jq '.findings[] | select(.consequence == "rolls")'
idem ./charts -o json | conftest test -
```

The one exception proves the rule: `idem` *reads* the suppression config you already keep for
ArgoCD and Flux, because that is a fact about your estate rather than a second place to
configure `idem`.

### No `--type` flag, ever

If the user has to tell the tool what it is looking at, the tool should have looked.
`chartref.Classify` decides between a local path, an OCI reference, `--repo` and a repository
alias by inspecting the reference and the disk. A directory named `doctor` is a chart, not the
verb, for the same reason.

### No HTML, CSV or SARIF output

`text` for people, `json` and `yaml` for machines, `markdown` for a pull-request comment,
`github` for inline annotations. Each has a reader who cannot use the others, and SARIF in
particular implies a line number `idem` deliberately refuses to guess (§9).

**`yaml` was added later, and it bends this rule rather than following it.** It is not a format
with a reader `json` cannot serve — `idem -o json | yq -P` produced the same thing before it
existed. What it buys is that the audience is Kubernetes operators, the remediation entries end
up in YAML manifests, and `yq` is as common here as `jq`. The cost is one more rendering to keep
correct, and that cost is paid structurally rather than by discipline: both formats encode the
same `contract()` value, and a test decodes both and requires deep equality. Note that
`gopkg.in/yaml.v3` does **not** read `json` struct tags — without the mirrored `yaml` tags every
key would silently lowercase into a different contract.

### Why the `ignoreDifferences` block carries a caveat — and only sometimes

This one was written wrong first, and the correction is worth keeping.

The original claim was that under `ServerSideDiff=true` the ignore normalizer "never sees the
rendered config at all", so every emitted block might be inert. **Checked against
`gitops-engine/pkg/diff/diff.go`, that is not what happens.** Server-side diff runs a Server-Side
Apply dry-run and compares the result with live — and the normalizer *is* applied, to both that
predicted-live object and the live one. Only the pre-processing pass is skipped, via
`WithSkipFullNormalize(true)`. A pointer at a field the chart renders still addresses it.

So the caveat was casting doubt on blocks that work, on every run, which is how a caveat stops
being read.

**One pointer genuinely does depend on the mode: `/stringData/KEY`.** It exists to stop `selfHeal`
overwriting the value on the `RespectIgnoreDifferences` sync path, which applies pointers to the
raw target — the only place `stringData` still exists, because the API server never stores it.
Under server-side diff the predicted-live object has only `data`, so `/stringData` addresses
nothing, and silently, exactly as a wrong ArgoCD pointer does. The block emits both pointers and
says so, only when it contains one.

idem cannot detect the mode and does not try: the global switch is `controller.diff.server.side`
in `argocd-cmd-params-cm`, which is not in any manifest idem reads, and the per-application
`argocd.argoproj.io/compare-options` annotation being absent does not mean the mode is off. The
caveat is earned by what the block *contains*, not by a guess about the cluster — §9's rule.

(`ServerSideApply=true` is a different option on a different code path and does not affect these
pointers — the two are routinely conflated.)
