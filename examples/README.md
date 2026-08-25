# examples

Two tiny charts to try `idem` on.

```sh
idem ./examples/churning-chart    # a chart that cannot converge under ArgoCD
idem ./examples/stable-chart      # the same shape, pinned
```

`churning-chart` generates a password with nothing to stabilise it, so every render produces a
different Secret. `stable-chart` is the same shape with the value pinned in `values.yaml`.

Neither chart contains a `{{ lookup }}`, and that is deliberate: it is why `idem` reports
**CHURNS for all three engines** here rather than the split verdict. With no `lookup` anywhere,
nothing can stabilise the value under Flux or Helm either, so this is a chart defect rather than
an ArgoCD limitation — which is exactly the conclusion `idem` prints.

The split verdict — churning under ArgoCD, stable under Flux and Helm — is what a `lookup` guard
produces, because ArgoCD's repo-server renders from git with no cluster access and resolves
`lookup` to `{}`. That case needs a cluster to demonstrate, so it lives in `testdata/guarded`
rather than here.
