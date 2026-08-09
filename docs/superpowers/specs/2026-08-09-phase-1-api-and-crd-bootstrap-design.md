# Phase 1, part 1 — `api/v1alpha1` and CRD bootstrap

- **Status:** approved
- **Date:** 2026-08-09
- **Covers:** roadmap phase 1, first bullet — "`api/v1alpha1` scaffolding; CRDs embedded in
  the operator binary and applied on every start"

## Context

Phase 1 has four deliverables: the API types, Flux bootstrap, the `Platform`/`PackageSource`/
`Package` reconcilers, and the `packages/**` → OCI publishing pipeline. This spec covers the
first one only. It stops short of any reconciler.

The cut is deliberate. The types plus an applier is the smallest increment that runs
end-to-end and can be judged: a binary you can point at a cluster, that installs its own CRDs
and tells you whether they took. Types alone would compile without ever proving the schema is
acceptable to an API server.

## Prior art

The shape below is informed by three existing systems rather than invented.

**OpenShift `ClusterVersion`** is the closest analogue to `Platform`. `spec.desiredUpdate` is
a single pin; `status.desired` is what the cluster is working toward; `status.history[]` is an
append-only record, newest first, each entry carrying `state: Completed|Partial` with start
and completion times. Conditions are `Available`, `Progressing`, `Degraded`.

We diverge on one point deliberately: **OpenShift does not support rollback.** Our phase-1
done-when requires that rolling a version back works, so the design treats the version field
as freely movable in both directions rather than as a ratchet.

**Flux `OCIRepository`** resolves `spec.ref` with the precedence `digest > semver > tag`, and
records in `status.artifact.revision` what a tag actually resolved to. Keeping "what was
asked for" separate from "what was resolved" is what makes a rollback auditable instead of
hopeful, and `Platform.status.current` copies that split.

**Flux `dependsOn`** with `wait: true` is the mechanism behind "two-stage OCI repositories so
migrations land before component upgrades". `Package.spec.stage` exists to drive it.

## API design

Group `platform.paas.io/v1alpha1`. All three kinds are cluster-scoped, per architecture.md §2.

### `Platform`

A singleton. The name is constrained to `cluster` by a CEL rule, because architecture.md §6
says the platform version is pinned in *a single* `Platform` CR, and an API that permits two
of them invites a split brain that nothing else in the system is designed to resolve.

```yaml
apiVersion: platform.paas.io/v1alpha1
kind: Platform
metadata:
  name: cluster
spec:
  version: v1.4.2                          # the pin; one field change is the upgrade
  registry: oci://registry.paas.io/paas
status:
  observedGeneration: 3
  current:
    version: v1.4.2
    digest: "sha256:…"                     # what the tag resolved to
  history:                                 # newest first, capped at 10 entries
    - version: v1.4.2
      digest: "sha256:…"
      state: Completed                     # Completed | Partial
      startedTime: "2026-08-09T18:00:00Z"
      completionTime: "2026-08-09T18:04:11Z"
  conditions: []                           # Available, Progressing, Degraded
```

The cap is enforced by the reconciler that writes it, not by validation, and is set at ten:
enough to cover a bad upgrade and the rollback after it, few enough that the object stays
readable. Trimming the oldest entry is a status write like any other.

`status.history` is not required by the done-when and is the first thing to cut if this proves
too large. It is included because rollback is the phase's headline feature, and a rollback
without a record of what you were previously running is a guess. It is status-only, so
removing or adding it later is not an API break.

### `PackageSource`

Where artifacts come from. The `Platform` reconciler will create this; it is not written by
hand in normal operation.

```yaml
spec:
  url: oci://registry.paas.io/paas         # no tag, per OCIRepository convention;
                                           # copied verbatim from Platform.spec.registry
  interval: 5m                             # defaulted, not required
  secretRef: {name: …}                     # optional
  insecure: true                           # the phase-0 registry speaks plain HTTP
status:
  observedGeneration: 1
  artifact: {revision: "v1.4.2@sha256:…", digest: "sha256:…"}
  conditions: []
```

### `Package`

One component at one version.

```yaml
spec:
  sourceRef: {name: platform}
  chart: cilium
  version: "1.18.12"
  stage: component                         # migration | component
  values: {}                               # runtime.RawExtension
status:
  observedGeneration: 1
  appliedDigest: "sha256:…"
  conditions: []
```

`stage` is an enum rather than a `isMigration` bool. go-guidelines calls for enums wherever a
third state is imaginable, and a pre-flight or verification stage is easy to imagine. It
carries the two-stage ordering in the API rather than hiding it in reconciler logic, so the
ordering is visible in `kubectl get package` rather than inferable only from source.

### Conventions

Per go-guidelines §"API types", without restating them here: kubebuilder markers are the
schema; optional scalars are pointers; status carries `[]metav1.Condition` plus
`observedGeneration`; status is written only by its controller; nothing secret goes in status.

`api/v1alpha1` imports `k8s.io/apimachinery` and nothing else. External clients import this
package, and every dependency added here becomes theirs.

## Generation

`controller-gen` produces `zz_generated.deepcopy.go` and the CRD YAML. It is run through
`go run` with its version pinned in `hack/versions.sh`, which is the only place a version may
live, and `versions_check` gains an entry so `make versions` catches a pin that stops
resolving.

Generated CRDs are written to `internal/crd/manifests/`, adjacent to the code that embeds
them. One copy, and it is the copy that ships inside the binary.

- `make generate` runs both controller-gen passes.
- The CI `unit` job's "generated files are current" step extends from `go mod tidy` alone to
  also run `make generate` and `git diff --exit-code`. A generated file that disagrees with
  its source is a build that lies about what it contains.

## The applier

`internal/crd` embeds the manifests and exposes a single entry point that server-side applies
every CRD and waits for each to report `Established`. `cmd/paas-operator` is thin: flag wiring
and a call into it.

Two decisions are load-bearing:

**Field manager `paas-operator/crd`, frozen from the first commit.** AGENTS.md non-negotiable
5 makes the field-manager string API; changing it later orphans field ownership across every
cluster in the fleet. It is scoped rather than bare `paas-operator` so the `Platform`
reconciler can take its own manager later without inheriting ownership of CRD fields.

**Apply with `Force: true`.** The operator owns these CRDs completely. Without force, a single
`kubectl edit` against a CRD leaves the operator wedged on a conflict it can never resolve on
its own — a worse and much quieter failure than overwriting a manual edit that should not
exist. The apply is level-triggered and idempotent: running it against an up-to-date cluster
changes nothing.

## Testing

### Unit — `make test`, under ten seconds, no cluster

- Deepcopy round-trips for each type.
- Parse the embedded manifests and assert every expected CRD is present and well-formed.
  This exists specifically to catch an `//go:embed` pattern that silently matches nothing,
  which otherwise produces a binary that installs zero CRDs and reports success.

### Integration — `make test-integration`, envtest, new in this phase

testing.md already anticipates this target and tier. Cases:

- Applying the embedded CRDs into a real API server leaves each one `Established`.
- A second apply is a no-op — the level-triggered claim, tested rather than asserted.
- A CRD mutated out from under the operator converges back on the next apply.
- `metadata.name: notcluster` is rejected by the singleton CEL rule.
- `stage: bogus` is rejected by the enum.

The last two assert the *specific* validation message, not merely that an error occurred.
AGENTS.md non-negotiable 6: a test that accepts any error keeps passing after the rule it
guards is deleted.

envtest requires `setup-envtest` and its API-server assets. Both get a pin in
`hack/versions.sh` and an entry in `hack/deps.sh`, and the tier gets its own CI job — a gate
that is not a CI job is not a gate.

### Coverage

`COVERAGE_MIN` is 95% and today only `pkg/wait` contributes. `zz_generated.deepcopy.go` is
large, mechanical, and generated. Left in the profile it either drags the percentage down or,
once round-trip tests exercise it, inflates it into meaninglessness — a coverage number made
mostly of generated code measures nothing about the code someone wrote.

`zz_generated.*` is therefore excluded from the coverage profile, so the floor keeps measuring
hand-written code. The floor itself does not move.

## Out of scope

Named so that scope reduction is explicit rather than discovered later:

- Every reconciler. `Platform` → `PackageSource`/`Package` is the next spec.
- Flux bootstrap.
- The `packages/**` → OCI publishing pipeline.
- `Tenant`, `App`, `Build`, `Domain`, `ServiceClass` types — phases 2 through 4.
- Conversion webhooks. There is one API version; a second one earns the machinery.

## Risks

**The types are being designed before the reconciler that consumes them.** Fields will
probably be wrong in ways only the reconciler reveals. This is acceptable at `v1alpha1` — the
version exists to say so — and the alternative, designing both at once, is the large spec this
one was cut down from.

**`values` as `RawExtension` is unvalidated at this layer.** The schema that constrains it
lives in the chart's `values.schema.json`, which arrives with the publishing pipeline. Until
then a `Package` can carry values no chart accepts, and nothing catches it until a reconciler
tries.
