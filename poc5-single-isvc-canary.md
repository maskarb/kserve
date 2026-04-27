# POC 5: Single-ISVC Canary with Explicit Canary Spec Array

## Overview

This POC implements canary rollout for InferenceService in RawDeployment mode
using a single ISVC with an embedded `canary` array. Based on the
[upstream maintainer proposal](https://github.com/kserve/kserve/issues/5335#issuecomment-4320814724),
each canary entry defines a name, predictor spec, and traffic percentage. The
controller creates canary deployments within the same ISVC, all serving under
the same model name. Traffic is split via Gateway API HTTPRoute weighted
backends (upstream) and OpenShift Route `alternateBackends` (omc/downstream).

## Branches

- KServe: [maskarb/kserve:canary-single-isvc-poc](https://github.com/maskarb/kserve/compare/canary-single-isvc-poc) (179 lines, 4 files)
- omc: [maskarb/odh-model-controller:canary-single-isvc-omc](https://github.com/maskarb/odh-model-controller/compare/canary-single-isvc-omc) (53 lines in Route reconciler)

## What Was Implemented

### 1. API Types (`pkg/apis/serving/v1beta1/inference_service.go`)

Added `Canary` field to `InferenceServiceSpec` and new `CanarySpec` type:

```go
type InferenceServiceSpec struct {
    Predictor   PredictorSpec    `json:"predictor"`
    Explainer   *ExplainerSpec   `json:"explainer,omitempty"`
    Transformer *TransformerSpec `json:"transformer,omitempty"`
    Canary      []CanarySpec     `json:"canary,omitempty"`
}

type CanarySpec struct {
    Name           string        `json:"name"`
    Predictor      PredictorSpec `json:"predictor"`
    TrafficPercent int32         `json:"trafficPercent"`
}
```

### 2. Predictor Component (`pkg/controller/v1beta1/inferenceservice/components/predictor.go`)

Added `reconcileCanaryDeployments()` method that runs after the main predictor
reconciliation in RawDeployment mode. For each canary entry:

1. Builds a pod spec from the same serving runtime as the stable predictor
2. Overrides `--model_name` to match the primary ISVC name (so all variants
   serve under the same model identity)
3. Sets the `internal.serving.kserve.io/storage-initializer-sourceuri`
   annotation so the storage initializer webhook injects the init container
   to download the canary's model
4. Creates metadata with labels for `serving.kserve.io/inferenceservice`,
   `component: predictor`, and the raw deployment app label
5. Creates the deployment and service via `NewRawKubeReconciler` with owner
   references back to the ISVC

Naming convention: `{isvc-name}-{canary-name}-predictor`

Example: ISVC `sklearn` with canary `v2` creates:
- Deployment: `sklearn-v2-predictor`
- Service: `sklearn-v2-predictor`

### 3. HTTPRoute Reconciler (`pkg/controller/v1beta1/inferenceservice/reconcilers/ingress/httproute_reconciler.go`)

Added `applyCanaryWeights()` function that modifies HTTPRoute rules to include
weighted backends. Called from both `createRawPredictorHTTPRoute()` and
`createRawTopLevelHTTPRoute()` when `len(isvc.Spec.Canary) > 0`.

For each rule:
1. Uses the existing backendRef as a template for kind, group, namespace, and port
2. Sets the stable backend's weight to `100 - sum(canary percentages)`
3. Adds a weighted backend for each canary entry using the naming convention
   `{isvc-name}-{canary-name}-predictor`

### 4. omc Route Reconciler (`internal/controller/serving/reconcilers/kserve_raw_route_reconciler.go`)

Added `applyCanaryTrafficSplits()` method on the existing `KserveRawRouteReconciler`.
When `isvc.Spec.Canary` has entries:

1. Calculates stable weight as `100 - sum(canary percentages)`
2. Sets `route.Spec.To.Weight` to the stable weight
3. Verifies each canary service exists via client lookup
4. Adds each canary service as an `alternateBackend` with its traffic percentage
5. Resolves `targetPort` to numeric (container port 8080) so HAProxy can route
   to all backends — each service uses a different named port, but the container
   port is the same

Also updated `route_comparator.go` to include `AlternateBackends` in the
comparison so changes to traffic splits trigger Route updates.

## User Workflow

### Initiate Canary

```yaml
apiVersion: serving.kserve.io/v1beta1
kind: InferenceService
metadata:
  name: sklearn
spec:
  predictor:
    model:
      modelFormat:
        name: sklearn
      storageUri: "gs://kfserving-examples/models/sklearn/1.0/model"
  canary:
    - name: v2
      predictor:
        model:
          modelFormat:
            name: sklearn
          storageUri: "gs://kfserving-examples/models/sklearn/2.0/model"
      trafficPercent: 20
```

Result: stable predictor gets 80% traffic, canary `v2` gets 20%.

### Increase Canary Traffic

Update `trafficPercent` to 50:

```yaml
  canary:
    - name: v2
      predictor:
        model:
          modelFormat:
            name: sklearn
          storageUri: "gs://kfserving-examples/models/sklearn/2.0/model"
      trafficPercent: 50
```

The controller updates HTTPRoute weights immediately. No pod restart needed.

### A/B Testing (Multiple Canaries)

```yaml
  canary:
    - name: v2
      predictor:
        model:
          modelFormat:
            name: sklearn
          storageUri: "gs://models/v2"
      trafficPercent: 10
    - name: v3
      predictor:
        model:
          modelFormat:
            name: sklearn
          storageUri: "gs://models/v3"
      trafficPercent: 10
```

Result: stable gets 80%, v2 gets 10%, v3 gets 10%.

### Promote

Update the stable model URI and remove the canary block:

```yaml
spec:
  predictor:
    model:
      modelFormat:
        name: sklearn
      storageUri: "gs://kfserving-examples/models/sklearn/2.0/model"
  # canary: removed
```

### Rollback

Remove the canary block without changing the stable model:

```yaml
spec:
  predictor:
    model:
      modelFormat:
        name: sklearn
      storageUri: "gs://kfserving-examples/models/sklearn/1.0/model"
  # canary: removed
```

## Replica Management Design

The upstream proposal describes constant GPU usage during rollout — stable
replicas scale down as canary replicas scale up. To avoid requiring users to
manually calculate replica distributions, the controller should derive replica
counts from the total `minReplicas` and `trafficPercent`.

### How It Would Work

The user specifies total replicas on the stable predictor and only
`trafficPercent` on the canary. The controller computes the distribution:

```yaml
spec:
  predictor:
    model:
      storageUri: "hf://meta-llama/Llama-3-8B"
    minReplicas: 4
  canary:
    - name: v2
      predictor:
        model:
          storageUri: "hf://meta-llama/Llama-3.1-8B"
      trafficPercent: 25
```

**Automatic mode** (canary does not set `minReplicas`):

| `trafficPercent` | Stable replicas | Canary replicas | Total |
|------------------|-----------------|-----------------|-------|
| 0                | 4               | 0               | 4     |
| 25               | 3               | 1               | 4     |
| 50               | 2               | 2               | 4     |
| 75               | 1               | 3               | 4     |
| 100              | 0               | 4               | 4     |

**Explicit mode** (canary sets `minReplicas`): The canary's replica count is
subtracted from the stable's total. This overrides the traffic-based
calculation and avoids rounding ambiguity.

```yaml
spec:
  predictor:
    minReplicas: 4        # total budget
  canary:
    - name: v2
      predictor:
        minReplicas: 1    # explicit: take 1 from the total
      trafficPercent: 20
```

| Stable `minReplicas` | Canary `minReplicas` | Stable replicas | Canary replicas | Total |
|----------------------|---------------------|-----------------|-----------------|-------|
| 4                    | 1                   | 3               | 1               | 4     |
| 4                    | 2                   | 2               | 2               | 4     |
| 4                    | 4                   | 0               | 4               | 4     |

When canary `minReplicas` is set, the controller uses it directly instead of
deriving from `trafficPercent`. The traffic percentage still controls the
HTTPRoute weights — replica count and traffic percentage are independent
concerns in this mode.

At 100%, the stable has 0 replicas. The user then promotes by updating the
stable `storageUri` and removing the canary block. The controller scales stable
back to 4 — the model artifacts are likely already cached on the nodes from the
canary pods, so startup is fast.

### Promotion Flow

1. User ramps `trafficPercent` to 100 — stable at 0 replicas, canary at 4
2. User observes canary metrics and confirms the new model is healthy
3. User promotes: updates stable `storageUri` to match canary, removes `canary` block
4. Controller scales stable to 4 replicas with the new model URI
5. Controller deletes the canary deployment
6. During step 4, stable pods start up (fast — model cached on node)
7. No downtime: canary pods continue serving until stable pods are ready, then
   the deployment's rolling update strategy handles the transition

### HPA Interaction

If the stable predictor has an HPA defined and the user adds a canary, the
canary's replicas should be subtracted from the stable's `minReplicas`, not
from the HPA's current replica count. If HPA has scaled stable to 10 because
of load, forcibly removing 2 replicas would fight the autoscaler.

Example: stable has `minReplicas: 4`, `maxReplicas: 10`, HPA currently at 10.
User adds canary with `minReplicas: 2`:

- Stable `minReplicas` adjusted to 2, `maxReplicas` stays at 10
- HPA continues managing stable between 2-10 based on load
- Canary gets 2 fixed replicas (no HPA)
- Total floor: 4 (2 stable min + 2 canary fixed) — same minimum as before
- Under load: up to 12 (10 stable + 2 canary) — canary adds fixed overhead

The GPU budget isn't strictly constant under load, but the alternative —
forcibly scaling down an HPA-managed deployment — is worse. The canary is a
fixed cost the user opts into.

For the traffic-percent-only case (no explicit canary `minReplicas`), the
canary's replica count is derived from the stable's `minReplicas`, not the
HPA's current count. 25% of `minReplicas: 4` = 1 canary replica, regardless
of whether HPA has stable at 10.

Canary deployments should not have their own HPA. See "Canary Spec Scope"
below.

### Edge Cases

**1 total replica**: The controller cannot split a single replica between
stable and canary. Options:
- Temporarily scale to 2 (1 stable + 1 canary) — breaks the constant GPU
  budget but enables canary testing. This may be the only practical choice
  for single-replica deployments.
- Reject canary configuration via validating webhook when `minReplicas < 2`.
- Allow it and set stable to 0, canary to 1 — but this provides no fallback
  if the canary fails.

**Rounding**: 4 replicas at 30% canary = 1.2 canary replicas. The controller
must round, and any rounding policy creates a mismatch between traffic
percentage and replica capacity. For example, rounding down to 1 canary + 3
stable means 25% of pods serve 30% of traffic — the canary pods handle more
load per replica. Options:
- Round to nearest, minimum 1 canary when `trafficPercent > 0`
- Document that traffic percentage and replica distribution are approximate

**0% canary traffic**: User sets `trafficPercent: 0` but keeps the canary
block. This is useful for pre-staging: deploy the canary model (download
artifacts, warm up) without sending traffic. The controller should keep the
canary deployment at 0 replicas and not add it to the HTTPRoute backends.

**100% canary traffic, not yet promoted**: All replicas are canary, stable is
at 0. If the canary crashes, there is no fallback — stable has 0 replicas.
Options:
- Keep stable at minimum 1 replica as a safety net (breaks exact GPU budget)
- Accept the risk — the user chose 100% canary, they own the blast radius
- Add a `safetyReplicas` field to let users control this

**Multiple canaries**: 4 replicas, canary-a at 25%, canary-b at 25%. That
works: 1 + 1 + 2 = 4. But at 33%/33%: 4 * 0.33 = 1.32 each — rounding both
up to 2 means 2 + 2 + 0 stable = 4, but stable has 0 replicas with only 34%
of traffic going to it. Rounding interactions with multiple canaries need
careful design.

**HPA interaction**: If HPA manages replicas, the controller's replica math
conflicts with autoscaler decisions. The upstream proposal suggests HPA
manages only stable while canary stays at fixed replicas. With
controller-managed distribution, the HPA target would need to be the total
replica count, with the controller partitioning the HPA's decision across
stable and canary. This is complex and likely a follow-up.

## Canary Spec Scope

The POC uses the full `PredictorSpec` for the canary, which means users could
configure autoscaling, batching, logging, timeouts, deployment strategy, labels,
and annotations on the canary. Most of these don't make sense for a canary
variant and could lead to confusing behavior (e.g., an HPA on the canary
fighting the controller's replica management).

A production implementation should use a narrower spec that only exposes what's
actually supported:

```go
type CanarySpec struct {
    Name           string     `json:"name"`
    Model          ModelSpec  `json:"model"`
    MinReplicas    *int32     `json:"minReplicas,omitempty"`
    TrafficPercent int32      `json:"trafficPercent"`
}
```

Everything else — autoscaling, batching, logging, timeouts, deployment strategy,
container resources, service account — should be inherited from the stable
predictor. This makes it clear that a canary is a model variant, not an
independent deployment with its own operational configuration.

If the canary needs different container resources (e.g., a larger model version
requiring more memory), a `resources` field could be added. But autoscaling,
batching, and logging should not be configurable per-canary.

## Demonstration Results

### Kind Cluster (Gateway API, Envoy Gateway)

Environment: kind v1.35.0, cert-manager v1.17.0, Envoy Gateway v1.6.3,
Gateway API v1.4.1, kserve controller built from this branch.

**80/20 split (500 requests via envoy access logs):**
```
Stable (10.244.0.19): 391 (80.2%)
Canary (10.244.0.20): 96 (19.7%)
Total: 487
```

**50/50 split (500 requests, after updating trafficPercent to 50):**
```
Stable (10.244.0.19): 236 (50.6%)
Canary (10.244.0.20): 230 (49.3%)
Total: 466
```

HTTPRoute weights updated immediately upon ISVC spec change — no pod restart,
no downtime.

### CRC Cluster (OpenShift Routes, omc)

Not tested with this POC. The omc branch (`canary-single-isvc-omc`) was built
and compiles cleanly, but the CRC testing in this session used the Option 4
cross-ISVC `router.route.splits` approach instead. That test validated the omc
Route reconciler mechanics (alternateBackends, numeric targetPort) with 500
requests showing exact 80/20 distribution on OpenShift 4.21. The same omc
reconciler pattern applies here — the only difference is reading from
`isvc.Spec.Canary` instead of `isvc.Spec.Router.Route.Splits`.

## Key Design Decisions

### Model Name Override

The canary deployment's model server is configured with `--model_name={isvc-name}`
(the stable ISVC's name, not the canary's). This is critical — without it, the
model server rejects requests for a model name it doesn't recognize. This was
discovered during Option 4 (cross-ISVC) testing where two separate ISVCs had
different model names by default.

With the single-ISVC approach, the controller handles this automatically. The
user never needs to think about model name consistency.

### Storage Initializer Injection

The canary deployment sets the annotation
`internal.serving.kserve.io/storage-initializer-sourceuri` with the canary's
`storageUri`. The KServe mutating webhook detects this annotation on pod creation
and injects the storage initializer init container. This is the same mechanism
used for the stable predictor.

### OpenShift Route targetPort

When using `alternateBackends` on OpenShift Routes, the single `targetPort` must
resolve on all backends. Each predictor service names its port differently
(e.g., `sklearn-predictor` vs `sklearn-v2-predictor`), so a named port only
matches the primary service. The omc reconciler resolves to the numeric container
port (8080) when canary splits are configured.

### HTTPRoute Weight Calculation

Stable weight is computed as `100 - sum(canary percentages)`. If a user
specifies canaries totaling more than 100%, no validation prevents this in the
POC. The real implementation needs a validating webhook.

## What Is Missing

### 1. Canary Resource Cleanup on Promotion/Rollback

When the `canary` array is removed from the ISVC spec, the controller does not
delete the orphaned canary deployments and services. They remain in the cluster
with their owner reference pointing back to the ISVC, so they will be garbage
collected when the ISVC itself is deleted — but they won't be cleaned up on
promotion or rollback.

**Fix:** The predictor reconciler needs to list existing canary deployments
(by label `serving.kserve.io/inferenceservice={name}` and
`component=predictor`), compare against the current `canary` array, and delete
any that are no longer referenced.

### 2. HTTPRoute Cleanup on Promotion/Rollback

When the `canary` array is removed, the desired HTTPRoute is built with a
single unweighted backend. However, the `semanticHttpRouteEquals` comparator
uses `DeepDerivative`, which treats missing fields as matching. This means the
desired (single backend, no weight) is seen as a subset of the existing (two
backends with weights), and no update is triggered.

**Fix:** The comparator needs to use `DeepEqual` instead of `DeepDerivative`
for the `Rules` field, or the HTTPRoute reconciler needs explicit logic to
detect and remove stale backends.

### 3. Constant GPU Budget (Replica Scaling)

The upstream proposal describes scaling down stable replicas as canary replicas
scale up, keeping total GPU usage constant. This POC does not implement replica
management — both stable and canary run at their configured replica counts
independently.

**Fix:** When canary is configured, the controller should adjust stable
`replicas` to `totalReplicas - sum(canary replicas)`. This interacts with
autoscaling and needs careful design.

### 4. Validation Webhooks

No validation is performed on the `canary` spec. The real implementation needs:

- Canary `trafficPercent` values must be positive and sum to less than 100
- Canary `name` must be unique within the array
- Canary `name` must be a valid DNS label (used in deployment/service names)
- Canary predictor must specify a model (no empty specs)
- Canary should not be allowed with Knative deployment mode

### 5. Status Reporting

The ISVC status does not report canary-specific information. The real
implementation should add:

- Per-canary deployment readiness
- Per-canary traffic percentage (actual vs configured)
- Canary promotion/rollback state

The existing `LatestRolledoutRevision` and `PreviousRolledoutRevision` status
fields could potentially be reused for canary tracking.

### 6. Transformer/Explainer Canary

This POC only handles predictor canary. If an ISVC has a transformer or
explainer, the canary should also create corresponding transformer/explainer
deployments with the canary model. This adds complexity to the routing — the
top-level HTTPRoute routes to transformer (if present), and the transformer
routes to the predictor. Canary traffic splitting would need to be applied at
the transformer level as well.

### 7. Session Affinity

With per-request weighted routing, a user's requests may alternate between
stable and canary across turns in a conversation. For LLM use cases, this can
produce inconsistent behavior. Gateway API's `sessionPersistence` (experimental,
on HTTPRouteRule) could pin users to a version. This is a follow-up concern.

### 8. Canary-Specific Serving Runtime

The POC inherits the serving runtime from the stable predictor. If the canary
needs a different runtime (e.g., a newer version of vLLM), the canary spec
would need to support runtime selection independently. The current `CanarySpec`
includes the full `PredictorSpec`, so this is structurally possible but not
tested.

### 9. Metrics and Observability

Both stable and canary deployments are independently monitorable via existing
Prometheus metrics (each pod reports its own metrics). However, there is no
built-in way to compare canary vs stable performance side-by-side. A follow-up
could add canary-specific labels to ServiceMonitor/PodMonitor resources for
easier dashboard filtering.

### 10. OpenShift Route Cookie-Based Splitting

OpenShift Routes use sticky session cookies by default. For canary testing where
each request should be independently routed by weight, the Route may need
`haproxy.router.openshift.io/disable_cookies: "true"` and
`haproxy.router.openshift.io/balance: roundrobin` annotations. The POC does not
set these — in testing on CRC, traffic was distributed correctly without them
because `curl` does not send cookies. Browser-based clients would need these
annotations to avoid sticky routing.

### 11. Multi-Container Pod Support

The model name override logic iterates over container args looking for
`--model_name` prefixes. This assumes a single inference container. If the pod
has multiple containers (e.g., collocation mode with predictor + transformer),
the override logic needs to be more targeted.

## References

- Upstream maintainer proposal: [kserve#5335 comment](https://github.com/kserve/kserve/issues/5335#issuecomment-4320814724)
- Design decision: [RHOAIENG-56732](https://redhat.atlassian.net/browse/RHOAIENG-56732)
- Epic: [RHOAIENG-56574](https://redhat.atlassian.net/browse/RHOAIENG-56574)
- POC task: [RHOAIENG-60256](https://redhat.atlassian.net/browse/RHOAIENG-60256)
- Upstream issues: [kserve#4074](https://github.com/kserve/kserve/issues/4074), [kserve#2649](https://github.com/kserve/kserve/issues/2649), [kserve#1324](https://github.com/kserve/kserve/issues/1324)
- RFE: [RHAIRFE-78](https://issues.redhat.com/browse/RHAIRFE-78)
