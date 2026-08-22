# examples

Two tiny charts to try `idem` on.

```sh
idem ./examples/churning-chart    # a chart that cannot converge under ArgoCD
idem ./examples/stable-chart      # the same shape, pinned
```

`churning-chart` generates a password with nothing to stabilise it, so every render produces a
different Secret. Under `helm upgrade` that is survivable, because Helm stores the release and
the value only changes when you actually upgrade. ArgoCD's repo-server renders from git with no
cluster access, so it produces a new password every time it renders and the app can never reach
`Synced`.

The usual fix is a `{{ lookup }}` guard, which works under Flux and Helm and does nothing under
ArgoCD. That is the difference `idem` reports.
