# Go guidelines

> Conventions for this repository. Test strategy lives in [testing.md](testing.md).
> Where this document is silent, [Effective Go](https://go.dev/doc/effective_go) and the
> [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments) apply.

Every rule here exists because breaking it causes a specific failure in *this* system. Where
that isn't obvious, the failure is named.

## Layout

```
api/<group>/v1alpha1/ API types only, one package per API group. No business logic,
                      no imports from internal/.
cmd/<binary>/         main() and flag wiring. Thin — everything real lives below.
internal/controller/  one package per reconciled kind
internal/dynamic/     ServiceClass -> CRD generation, dynamic HelmRelease reconciler
pkg/tenancy/          tenant tree resolution. The only place the ancestor walk exists.
pkg/                  code we would accept an external importer for
test/e2e/             //go:build e2e
```

`api/` must stay importable by external clients — a tenant writing a Go operator against our
CRDs imports it. Keep its dependency set to `k8s.io/apimachinery` and nothing more.

`internal/` for everything that isn't a deliberate public contract. Moving a package from
`internal/` to `pkg/` later is easy; the reverse is a breaking change.

## API types

- Kubebuilder markers are the schema. `+optional`, `+kubebuilder:validation:*`, and
  `+kubebuilder:default` are load-bearing, not decoration.
- **Pointers for optional scalars.** `Replicas *int32` distinguishes unset from zero.
  `Replicas int32` cannot, and "scale to zero" versus "field omitted" is a real distinction in
  the `App` API.
- **Enums, not booleans**, for anything that might grow a third state. `isolation: shared` was
  a bool named `dedicated` in an earlier draft; adding `dedicated-nodes` would have required
  an API break.
- Immutability via CEL: `+kubebuilder:validation:XValidation:rule="self == oldSelf"`. Tenant
  name and parent are immutable — re-parenting is a migration, not an update
  ([ADR 0004](adr/0004-tenant-hierarchy-and-inheritance.md)).
- Status carries `[]metav1.Condition` with the standard reasons, plus `observedGeneration`.
  A status without `observedGeneration` cannot express "I have not looked at your change
  yet", and every consumer then races.
- Never put a secret, a token, or a password in status. Status is world-readable to anyone
  with get on the kind.
- Status is written only by its controller. Spec is written only by users.

## Errors

- Wrap with context: `fmt.Errorf("render helmrelease for %s: %w", key, err)`. The chain should
  read as a path from symptom to cause without opening a file.
- Inspect with `errors.Is` / `errors.As`. Never compare error strings; never `switch` on
  `err.Error()`.
- Sentinel errors are exported only when a caller must branch on them.
- **No `panic` outside `main` and package-level programmer errors.** A panic in a reconciler
  takes down the manager and every other controller sharing it.
- Do not log and return the same error. Pick one — the caller decides. Double-logged errors
  make an incident's log volume triple at the worst moment.
- `apierrors.IsNotFound(err)` on a get in a reconciler means the object is gone; return
  cleanly, do not requeue.

## Context

- `ctx context.Context` is the first parameter of anything that does I/O. Never store one in
  a struct.
- Every client call gets the reconcile `ctx`. Never `context.Background()` inside a reconciler
  — it survives manager shutdown and writes to a cluster the process is leaving.
- Threading `ctx` through constructors is required, not optional: it is what lets tests pass
  `t.Context()` and use `testing/synctest`.

## Logging

- `logr` via `ctrl.LoggerFrom(ctx)`. Structured key-values only, never `fmt.Sprintf` into the
  message.
- Levels: `Error` for something needing a human; `Info` (V0) for state changes; `V(1)` for
  reconcile detail; `V(2)` for per-object trace. Default deployment runs at V0 — anything
  logged per-reconcile at V0 will drown the cluster at 200 tenants.
- Always include `tenant` and the object key. A log line that doesn't say which tenant it
  concerns is unusable during an incident.
- Never log secret values, kubeconfigs, connection strings, or full HelmRelease values —
  values carry tenant credentials.

## Concurrency

- Prefer no goroutine. controller-runtime already gives you concurrency via
  `MaxConcurrentReconciles`; a hand-rolled goroutine inside a reconciler bypasses its rate
  limiting and error handling.
- Every goroutine has a documented owner and exit path, and honours `ctx`.
- `errgroup` for concurrent I/O with a shared error.
- Guard shared state with a mutex, or don't share it. `-race` runs in every CI job.
- No package-level mutable state. It defeats parallel tests and hides ordering bugs.

## Controllers

- **Reconcile is level-triggered and idempotent.** Compute desired state from the world as it
  is; never from what a previous pass did. A reconciler that reads its own prior side effects
  will diverge under retry.
- No side effects outside the cluster on a code path that can retry, unless guarded by a
  recorded state — a build must not be launched twice because a status write conflicted.
- **Return, don't loop.** `return ctrl.Result{Requeue: true}, nil` or an error. Never poll
  inside `Reconcile`.
- Requeue with backoff for "not ready yet"; return an error only for genuine faults. Returning
  an error for an expected wait pollutes error metrics and triggers alerts.
- **Server-side apply with a stable field manager** for every derived object. This is the
  mechanism that makes derived HelmReleases authoritative
  ([ADR 0001](adr/0001-generated-crds-over-aggregated-api.md)); the field manager string is
  API, so changing it orphans field ownership across the fleet.
- Owner references for garbage collection wherever the objects are namespace-local; explicit
  finalizers where they are not, or where external cleanup is needed.
- Finalizers must be idempotent and must not block forever on a missing dependency. A stuck
  finalizer means a tenant that cannot be deleted, and the fix is manual surgery.
- Use `Owns()` and `Watches()` to trigger on children rather than resyncing on a timer.
- Set `RateLimiter` deliberately. The default is fine; the failure mode when it isn't is a
  self-inflicted API server outage.
- Emit events for user-visible transitions. Tenants read `kubectl describe`, not our logs.

## Dependencies

- Standard library first. `go-cmp` for tests, `logr`, `controller-runtime`, and the
  `k8s.io/*` set are the baseline.
- No assertion DSL, no dependency-injection framework, no ORM.
- A new direct dependency needs a sentence in the PR describing what it replaces.
- Pin `k8s.io/*` and `controller-runtime` to a matched pair; mismatches surface as confusing
  runtime panics rather than build errors.
- `govulncheck` runs in CI and blocks merge.

## Testing

Full strategy in [testing.md](testing.md). Style rules:

- Table-driven, `t.Run` subtests, `t.Parallel()` where there is no shared state.
- `cmp.Diff(want, got)` for comparisons, reported as `got/want`. No assertion library.
- Helpers call `t.Helper()`. Cleanup via `t.Cleanup`, not `defer`, so it survives helper
  boundaries.
- **No `time.Sleep`.** Use a fake clock, poll with a deadline, or `testing/synctest` (Go 1.25+
  provides a fake clock and deterministic scheduling inside a bubble). Pass `t.Context()` so
  cancellation propagates.
- Golden files in `testdata/`, regenerated by `make golden`, never hand-edited.
- Name tests for the behaviour: `TestTenant_ChildInheritsParentMonitoring`, not `TestTenant2`.
- A test asserting a denial must assert the *specific* denial — a 403, a policy drop — not
  merely that an error occurred, or it will keep passing after the feature disappears.
- Use `envtest` for anything involving server-side apply, admission, or defaulting. The fake
  client's approximation of field-manager conflict semantics is precisely what our derived-
  object model depends on, so testing it against a fake tests the fake.

## Tooling

```
gofumpt          formatting, stricter than gofmt
golangci-lint    govet, staticcheck, errcheck, ineffassign, misspell, revive, bodyclose
go vet           in CI separately, non-negotiable
govulncheck      blocks merge
```

`.golangci.yml` is where that list is enforced — including gofumpt, so formatting fails the
lint job rather than waiting for someone to run `make fmt`. It enables each linter explicitly
instead of relying on golangci-lint's defaults, which change between releases.

Generated code (deepcopy, CRDs, clients) is committed and CI verifies it is current:
`make generate && git diff --exit-code`. Reviewing a PR whose generated files are stale wastes
everyone's time.

## Style notes

- Package names are lowercase, singular, no underscores, and never `util`, `common`, or
  `helpers`. If a package needs one of those names it needs a different decomposition.
- Accept interfaces, return structs. Define interfaces at the consumer.
- Small interfaces. If it has more than three methods, ask what it's really for.
- No `init()`. Wire dependencies explicitly in `main`.
- Comments explain *why*. The code already says what.
- Exported identifiers have doc comments starting with the identifier name.
- Keep line length reasonable but do not fight the formatter over it.
