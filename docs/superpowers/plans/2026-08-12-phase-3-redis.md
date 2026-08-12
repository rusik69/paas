# redis — the second catalog entry Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A tenant applies a `Redis` in their namespace and gets a persistent Valkey they can write to and read back — added as a chart and a catalog template, with exactly one deliberate Go change.

**Architecture:** The `ServiceClass` machinery from phase 3 part 1 already turns a chart into a tenant-facing kind. This entry adds `packages/apps/redis` (a StatefulSet of one Valkey with a `volumeClaimTemplate`) and `packages/system/catalog/templates/redis.yaml`. The single Go change generalises `statusSchema()` so a service's status fields are declared by its own catalog template rather than hardcoded.

**Tech Stack:** Helm, Valkey, Kubernetes StatefulSet, Go (one function), envtest, Talos/KVM e2e.

**Design spec:** [docs/superpowers/specs/2026-08-12-phase-3-redis-design.md](../specs/2026-08-12-phase-3-redis-design.md). Read it before Task 1 — particularly "The one Go change, and why there is exactly one".

## Global Constraints

- **Read [docs/go-guidelines.md](../../go-guidelines.md) before the Go change.** Not optional.
- **Exactly one Go change is permitted: Task 1.** If any later task appears to need Go, that is a finding about the machinery — STOP and report it rather than absorbing it quietly. The whole point of this entry is the size of its diff.
- Pinned versions live only in `hack/versions.sh`, and `make versions` must check them. The chart carries the literal tag, mirroring how `KEYCLOAK_VERSION` and the vendored keycloakx chart already relate.
- **Write fewer comments.** Default to none. A comment earns its place only by explaining *why*. Doc comments on exported identifiers stay required.
- Smallest change that fully does the job. No speculative abstraction, no helper with one caller.
- `make test` stays under ten seconds and needs no cluster. No `time.Sleep` in tests.
- Negative tests assert the **specific** denial, never merely that an error occurred.
- Coverage floor is 95% per covered package, error paths included; an unreachable error path is deleted, not excused.
- Run `make verify` before every commit.
- **The chart must NOT carry `policy.paas.io/allow-to-apiserver`.** Valkey does not talk to the API server. Its absence is a deliberate assertion that the chart contract's API-server clause is a real opt-in — see the spec.

## Already true — do not rebuild

- The `ServiceClass` → CRD → per-kind controller → HelmRelease pipeline works and is proven.
- The chart-contract labels `paas.io/service-name` / `paas.io/service-namespace` are how a status watch maps an underlying object back to its CR. `readStatusFrom` selects on them.
- `readStatusFrom` renders JSONPath into a buffer and returns a **string**. Every `statusFrom` value is therefore already a string — this is why Task 1 needs no type system.
- Adding a `ServiceClass` needs no RBAC change: tenants already hold `apps.paas.io` with `resources: ["*"]`.

---

### Task 1: Declare status fields from spec.statusFrom

The only Go change in this plan. `statusSchema()` hardcodes a `primary` field; a structural schema silently drops anything undeclared on write, so redis writing `.status.ready` would produce nothing and say nothing.

**Files:**
- Modify: `internal/controller/serviceclass/crd.go` (`statusSchema`, and its one caller in `CRDFor`)
- Test: `internal/controller/serviceclass/crd_test.go`

**Interfaces:**
- Consumes: `v1alpha1.StatusSource` (fields `Path`, `From`, `JSONPath`).
- Produces: `func statusSchema(sources []v1alpha1.StatusSource) (apiextensionsv1.JSONSchemaProps, error)` — unexported; `CRDFor` propagates its error.

- [ ] **Step 1: Write the failing tests**

Add to `internal/controller/serviceclass/crd_test.go`. `testClass()` already exists and has a `statusFrom` of `.status.primary`:

```go
func TestCRDFor_DeclaresEachStatusFromPath(t *testing.T) {
	sc := testClass()
	sc.Spec.StatusFrom = []v1alpha1.StatusSource{{
		Path:     ".status.ready",
		From:     v1alpha1.ObjectRef{APIVersion: "apps/v1", Kind: "StatefulSet"},
		JSONPath: ".status.readyReplicas",
	}}

	crd, err := CRDFor(sc, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatalf("CRDFor: %v", err)
	}

	status := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]
	if _, ok := status.Properties["ready"]; !ok {
		t.Error("ready is not declared, so the API server would drop it on write")
	}
	if _, ok := status.Properties["primary"]; ok {
		t.Error("primary is still declared for a class that never mentions it")
	}
	for _, always := range []string{"observedGeneration", "conditions"} {
		if _, ok := status.Properties[always]; !ok {
			t.Errorf("%s is missing; it belongs to every generated kind", always)
		}
	}
}

func TestCRDFor_PostgresStatusPathStillDeclared(t *testing.T) {
	crd, err := CRDFor(testClass(), []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatalf("CRDFor: %v", err)
	}
	status := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]
	if _, ok := status.Properties["primary"]; !ok {
		t.Error("primary is no longer declared — this regresses the only shipped catalog entry")
	}
}

func TestCRDFor_NoStatusFromDeclaresOnlyTheCommonFields(t *testing.T) {
	sc := testClass()
	sc.Spec.StatusFrom = nil

	crd, err := CRDFor(sc, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatalf("CRDFor: %v", err)
	}
	status := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]
	if len(status.Properties) != 2 {
		t.Errorf("status has %d properties, want just observedGeneration and conditions", len(status.Properties))
	}
}

func TestCRDFor_RejectsAStatusPathItCannotDeclare(t *testing.T) {
	cases := []struct{ name, path, want string }{
		{"nested", ".status.a.b", ".status.a.b"},
		{"not under status", ".spec.thing", ".spec.thing"},
		{"bare", "primary", "primary"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := testClass()
			sc.Spec.StatusFrom = []v1alpha1.StatusSource{{
				Path:     tc.path,
				From:     v1alpha1.ObjectRef{APIVersion: "apps/v1", Kind: "StatefulSet"},
				JSONPath: ".status.x",
			}}

			crd, err := CRDFor(sc, []byte(`{"type":"object"}`))
			if err == nil {
				t.Fatal("CRDFor accepted a path it cannot declare; the field would be dropped silently on write")
			}
			if crd != nil {
				t.Error("CRDFor returned a CRD alongside its error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to name the offending path %q", err, tc.want)
			}
		})
	}
}
```

Add `"strings"` to the test imports if it is not already there.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/controller/serviceclass/... -run TestCRDFor -v`
Expected: FAIL — the `ready` case fails because only `primary` is declared, and the rejection cases fail because nothing rejects.

- [ ] **Step 3: Write the implementation**

Replace `statusSchema` in `internal/controller/serviceclass/crd.go`:

```go
// statusSchema is ours rather than the chart's: the conditions every generated
// kind carries, plus one field per statusFrom entry.
//
// Declared rather than free-form because a structural schema drops an
// undeclared field on write without erroring, so an undeclared status path
// would leave kubectl showing nothing and no message saying why.
func statusSchema(sources []v1alpha1.StatusSource) (apiextensionsv1.JSONSchemaProps, error) {
	props := map[string]apiextensionsv1.JSONSchemaProps{
		"observedGeneration": {Type: "integer", Format: "int64"},
		"conditions": {
			Type: "array",
			Items: &apiextensionsv1.JSONSchemaPropsOrArray{
				Schema: &apiextensionsv1.JSONSchemaProps{
					Type:     "object",
					Required: []string{"type", "status", "lastTransitionTime", "reason"},
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"type":               {Type: "string"},
						"status":             {Type: "string"},
						"observedGeneration": {Type: "integer", Format: "int64"},
						"lastTransitionTime": {Type: "string", Format: "date-time"},
						"reason":             {Type: "string"},
						"message":            {Type: "string"},
					},
				},
			},
		},
	}

	for _, s := range sources {
		field, ok := strings.CutPrefix(s.Path, ".status.")
		if !ok || field == "" || strings.Contains(field, ".") {
			return apiextensionsv1.JSONSchemaProps{}, fmt.Errorf(
				"statusFrom path %q must be .status.<field>", s.Path)
		}
		// Every value readStatusFrom produces is a string: it renders a
		// JSONPath into a buffer.
		props[field] = apiextensionsv1.JSONSchemaProps{Type: "string"}
	}

	return apiextensionsv1.JSONSchemaProps{Type: "object", Properties: props}, nil
}
```

In `CRDFor`, replace the `statusSchema()` call site so the error propagates. It currently reads `"status": statusSchema()` inside the schema literal, so hoist it above the literal:

```go
	status, err := statusSchema(sc.Spec.StatusFrom)
	if err != nil {
		return nil, fmt.Errorf("service class %s: %w", sc.Name, err)
	}
```

and use `"status": status` in the literal.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/controller/serviceclass/... -v`
Expected: PASS, including the pre-existing `TestCRDFor` and `TestCRDFor_StatusFromBecomesAPrinterColumn`.

- [ ] **Step 5: Verify and commit**

Run: `make verify`
Expected: green, `internal/controller/serviceclass` at or above its 95% floor.

```bash
git add internal/controller/serviceclass
git commit -m "Declare a generated kind's status fields from its own class"
```

---

### Task 2: The five files

The whole deliverable of this entry. **No Go.** If something here seems to need Go, stop and report it.

**Files:**
- Modify: `hack/versions.sh` (add `VALKEY_VERSION`, and a check for it in the versions command)
- Create: `packages/apps/redis/Chart.yaml`
- Create: `packages/apps/redis/values.schema.json`
- Create: `packages/apps/redis/values.yaml`
- Create: `packages/apps/redis/templates/valkey.yaml`
- Create: `packages/system/catalog/templates/redis.yaml`
- Modify: `packages/system/catalog/values.yaml`

**Interfaces:**
- Consumes: `statusSchema` from Task 1 (a `.status.ready` path is only declarable because of it).
- Produces: a chart named `redis` at version `0.1.0` in the registry, and a `ServiceClass` named `redis` generating kind `Redis`, plural `redises`.

- [ ] **Step 1: Pin Valkey**

Read `hack/versions.sh` first and match its style. Add alongside the other component versions:

```sh
VALKEY_VERSION="${VALKEY_VERSION:-8.1.1}"
```

Then add a check to the versions command beside the existing `_check_url` lines, so `make versions` catches the pin rotting:

```sh
	_check_url valkey "https://github.com/valkey-io/valkey/releases/tag/${VALKEY_VERSION}" || rc=1
```

Read how `_check_url` is called for `cnpg` and `keycloak` and match it exactly — the helper's signature and the `rc` handling are already established.

Run: `make versions`
Expected: valkey reports `ok`. If the tag does not resolve, pick the current Valkey 8 release and use that instead — do not leave a pin that fails the check.

- [ ] **Step 2: Write the chart metadata and values**

`packages/apps/redis/Chart.yaml`:

```yaml
apiVersion: v2
name: redis
description: A persistent Valkey instance for one tenant
type: application
version: 0.1.0
appVersion: "8.1.1"
```

`packages/apps/redis/values.yaml` — the tag repeats the pin, exactly as the vendored keycloakx chart repeats `KEYCLOAK_VERSION`:

```yaml
# Must equal VALKEY_VERSION in hack/versions.sh, which is what `make versions`
# checks still resolves upstream.
image: valkey/valkey:8.1.1

storage:
  size: 1Gi
  class: replicated-3

resources:
  cpu: 100m
  memory: 256Mi
```

- [ ] **Step 3: Write the schema — the security boundary**

`packages/apps/redis/values.schema.json`. This defines the ENTIRE writable surface a tenant has, because the CR's `.spec` becomes these values verbatim. Note there is no `image` key: the tenant may not choose one.

The bounds mirror what postgres settled on after review, and must survive `internal/schema.Convert`, which rejects any keyword it does not recognise:

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "properties": {
    "storage": {
      "type": "object",
      "properties": {
        "size": {
          "type": "string",
          "pattern": "^([1-9][0-9]{0,2}Mi|[1-5]Gi)$",
          "default": "1Gi",
          "description": "Volume size, capped at 5Gi. Every replicated storage class puts a full copy on each node it spans, out of a 20Gi data disk per dev-cluster node shared with every other volume."
        },
        "class": {
          "type": "string",
          "enum": ["replicated-2", "replicated-3"],
          "default": "replicated-3",
          "description": "StorageClass. Both are DRBD-replicated; the number is the replica count."
        }
      }
    },
    "resources": {
      "type": "object",
      "properties": {
        "cpu": {
          "type": "string",
          "pattern": "^([1-9]|[1-9][0-9]|[1-4][0-9]{2}|500)m$",
          "default": "100m",
          "description": "CPU request, capped at 500m — a quarter of a dev-cluster worker's 2 vCPUs."
        },
        "memory": {
          "type": "string",
          "pattern": "^([1-9][0-9]{0,2}Mi|1Gi)$",
          "default": "256Mi",
          "description": "Memory request, capped at 1Gi — a third of a dev-cluster worker's 3Gi."
        }
      }
    }
  }
}
```

- [ ] **Step 4: Add the schema to the conversion guard**

`internal/schema/structural_test.go` already has `TestConvert_PostgresChartSchema`, which loads the real postgres schema file and asserts specific constraints survived conversion. Add the equivalent for redis, following that test's exact shape — load `packages/apps/redis/values.schema.json` by relative path so it cannot drift, and assert the storage-class enum and at least one pattern survive.

This is a test file, not production Go, and it is required: without it a schema mistake surfaces only on a cluster.

Run: `go test ./internal/schema/... -v`
Expected: PASS.

- [ ] **Step 5: Write the chart template**

`packages/apps/redis/templates/valkey.yaml`. Every object carries the chart-contract labels; nothing carries `policy.paas.io/allow-to-apiserver`:

```yaml
{{- /*
  Labels on every object: the per-kind controller finds the StatefulSet by them
  to copy .status.readyReplicas into the Redis's own status.

  No policy.paas.io/allow-to-apiserver here, deliberately. Valkey never talks to
  the API server, and the tenant default-deny opt-in is per-pod so that a tenant
  running one controller does not thereby grant it to every workload.
*/ -}}
apiVersion: v1
kind: Service
metadata:
  name: {{ .Release.Name }}
  namespace: {{ .Release.Namespace }}
  labels:
    paas.io/service-name: {{ .Release.Name }}
    paas.io/service-namespace: {{ .Release.Namespace }}
spec:
  clusterIP: None
  selector:
    paas.io/service-name: {{ .Release.Name }}
  ports:
    - name: valkey
      port: 6379
      targetPort: valkey
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: {{ .Release.Name }}
  namespace: {{ .Release.Namespace }}
  labels:
    paas.io/service-name: {{ .Release.Name }}
    paas.io/service-namespace: {{ .Release.Namespace }}
spec:
  serviceName: {{ .Release.Name }}
  replicas: 1
  selector:
    matchLabels:
      paas.io/service-name: {{ .Release.Name }}
  template:
    metadata:
      labels:
        paas.io/service-name: {{ .Release.Name }}
        paas.io/service-namespace: {{ .Release.Namespace }}
    spec:
      containers:
        - name: valkey
          image: {{ .Values.image | quote }}
          args: ["--dir", "/data", "--appendonly", "yes"]
          ports:
            - name: valkey
              containerPort: 6379
          resources:
            requests:
              cpu: {{ .Values.resources.cpu | quote }}
              memory: {{ .Values.resources.memory | quote }}
            limits:
              memory: {{ .Values.resources.memory | quote }}
          volumeMounts:
            - name: data
              mountPath: /data
          readinessProbe:
            exec:
              command: ["valkey-cli", "ping"]
            initialDelaySeconds: 2
            periodSeconds: 5
  volumeClaimTemplates:
    - metadata:
        name: data
        labels:
          paas.io/service-name: {{ .Release.Name }}
          paas.io/service-namespace: {{ .Release.Namespace }}
      spec:
        accessModes: ["ReadWriteOnce"]
        storageClassName: {{ .Values.storage.class | quote }}
        resources:
          requests:
            storage: {{ .Values.storage.size | quote }}
```

`--appendonly yes` is what makes the PVC mean anything: without it Valkey holds everything in memory and the volume stores nothing.

- [ ] **Step 6: Write the catalog entry**

Add `redis: {version: "0.1.0"}` to `packages/system/catalog/values.yaml` alongside the postgres entry, then `packages/system/catalog/templates/redis.yaml`:

```yaml
apiVersion: platform.paas.io/v1alpha1
kind: ServiceClass
metadata:
  name: redis
spec:
  kind: Redis
  plural: redises
  chart:
    name: redis
    version: {{ .Values.redis.version | quote }}
  statusFrom:
    - path: .status.ready
      from:
        apiVersion: apps/v1
        kind: StatefulSet
      jsonPath: .status.readyReplicas
  ui:
    icon: redis
    category: databases
```

- [ ] **Step 7: Render and read the output**

Run: `helm template r packages/apps/redis` and `helm template c packages/system/catalog`

Check by eye, and say in your report that you did:
- every object in the redis chart carries both `paas.io/service-*` labels, including the `volumeClaimTemplate`;
- no object carries `policy.paas.io/allow-to-apiserver`;
- the `ServiceClass` renders `kind: Redis`, `plural: redises`, chart version `0.1.0`, and the `.status.ready` path.

- [ ] **Step 8: Verify and commit**

Run: `make verify`

```bash
git add hack/versions.sh packages internal/schema
git commit -m "Add redis to the catalog"
```

---

### Task 3: The e2e assertions

**For the `e2e-author` subagent** — it owns `test/e2e` and cannot run the suite.

**Files:**
- Create: `test/e2e/redis_test.go` (`//go:build e2e`)

**Interfaces:**
- Consumes: the helpers already in `test/e2e` — `setPlatformVersion`, `ensureRootNamespace`, `applyTenant`, `waitNamespace`, `dynClient`, `clientset`, `dumpNamespace`. Read `test/e2e/service_test.go` for their exact signatures and for `postgresFixture`, which the redis fixture should mirror.

- [ ] **Step 1: Write the tests**

Four assertions. Use `setPlatformVersion(t, "v0.1.0")` — **not v0.2.0**, which is a synthetic fixture built by `hack/operator.sh` that does not contain the catalog package.

```go
//go:build e2e

package e2e

// TestRedis_BecomesReadyAndReportsItsReplicaCount
//   Apply a Redis in a tenant namespace. Wait for Ready AND for .status.ready to
//   equal the StatefulSet's .status.readyReplicas — fold both into one wait, so
//   nothing asserts a stronger condition than it waited for. That equality is the
//   point; asserting non-emptiness alone would pass against a hardcoded value.

// TestRedis_StoresWhatWasWritten
//   The assertion postgres's suite never makes. Exec valkey-cli in the instance
//   pod: SET a key, GET it back, assert the value returned is the value written.
//   A pod that is Running and a datastore that actually stores are different
//   claims, and the second is the one a tenant is buying.

// TestRedis_OffSchemaFieldIsRejectedWithItsOwnMessage
//   storage.size "10Gi" exceeds the schema's 5Gi cap: assert the specific
//   validation message, not merely that an error occurred. And an undeclared
//   field (image) is refused by strict decoding, naming the field.

// TestRedis_DeleteReclaimsEverything
//   Delete the Redis; assert the HelmRelease, the StatefulSet and the PVC are all
//   gone. The PVC is the link worth asserting — a StatefulSet's volumeClaimTemplate
//   PVCs are NOT deleted with the StatefulSet by Kubernetes, so this asserts the
//   ownership chain through the HelmRelease actually reclaims them. If it does not,
//   that is a real finding about the chart, not a test bug: report it.
```

Write these as real Go tests with real assertions.

- [ ] **Step 2: Check it compiles**

Run: `make vet-e2e`
Expected: clean. The e2e suite is invisible to `make test`, so this is the only thing that catches a broken build.

- [ ] **Step 3: Commit**

```bash
git add test/e2e/redis_test.go
git commit -m "Prove a tenant's Redis runs, stores, validates and reclaims"
```

---

### Task 4: Verify the property, and record it

The actual deliverable of this entry.

**Files:**
- Modify: `docs/roadmap.md` (phase 3 charts bullet and Done-when)
- Modify: `docs/superpowers/specs/2026-08-12-phase-3-redis-design.md` (Status)

- [ ] **Step 1: Measure the diff**

Run, with `<base>` being the commit before Task 1:

```bash
git diff --stat <base>..HEAD -- '*.go'
```

Expected: `internal/controller/serviceclass/crd.go` and its test, plus the redis case added to `internal/schema/structural_test.go`, and **nothing else**. Report exactly what it shows.

If any other production Go changed, the claim did not hold. Say so plainly in the report and name the file — that finding is worth more than the service, and it must reach the roadmap rather than being quietly absorbed.

- [ ] **Step 2: Record it**

Update `docs/roadmap.md`'s charts bullet in the established `*(Landed: ...)*` style, naming the tests that prove redis and stating what the diff measurement showed. Mark `bucket` still outstanding. Set the design spec's Status to implemented, naming the tests.

Where the roadmap describes redis, say plainly that it is one instance with no failover — dev-cluster-grade — so it is not mistaken for a finished managed service.

- [ ] **Step 3: Verify and commit**

Run: `make verify && make actionlint`

```bash
git add docs
git commit -m "Record what the second catalog entry cost"
```

---

## Self-review

**Spec coverage:** the one Go change → Task 1. The five files → Task 2. Valkey pin and its version check → Task 2 Step 1. The schema as security boundary → Task 2 Step 3, guarded by Step 4. The deliberate absence of the API-server label → Task 2 Steps 5 and 7. `statusFrom` reading `readyReplicas` → Tasks 1 and 2. Functional SET/GET → Task 3. Reclaim → Task 3. The property itself → Task 4.

**Known risk carried into execution:** Task 1 touches the function every generated CRD depends on, so the e2e run that verifies redis must include the `TestService_*` tests rather than running the redis ones alone. A regression here breaks postgres, not redis. This is stated in the spec's Risks and is the expensive mistake to make.

**Type consistency:** `statusSchema` gains a `[]v1alpha1.StatusSource` parameter and an error return in Task 1; `CRDFor` is its only caller and is updated in the same task. The `.status.ready` path used in Task 2's catalog entry is exactly what Task 1's rejection rule permits (`.status.<field>`, single segment). The chart-contract label keys used in Task 2 match those `readStatusFrom` selects on.
