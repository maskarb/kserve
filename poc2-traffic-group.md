# POC 2: Traffic Group — Multi-ISVC Weighted Traffic Splitting

## Overview

This POC introduces a `trafficGroup` field on `InferenceServiceSpec` that allows
multiple independent InferenceServices to share a single ingress endpoint with
weighted traffic splitting via Gateway API HTTPRoute backends.

Unlike POC 1 (single ISVC with `canaryTrafficPercent`), each ISVC in a traffic
group has its own model version, Deployment, and Service. The controller creates
a shared HTTPRoute that distributes traffic across all group members by weight.

## What Was Implemented

Seven files were modified (219 lines added):

### 1. API Types (`pkg/apis/serving/v1beta1/inference_service.go`)

Added `TrafficGroupSpec` struct and `TrafficGroup` field to `InferenceServiceSpec`:

```go
type TrafficGroupSpec struct {
    Name           string `json:"name"`
    TrafficPercent int32  `json:"trafficPercent"`
}
```

### 2. DeepCopy (`pkg/apis/serving/v1beta1/zz_generated.deepcopy.go`)

Generated `DeepCopyInto` and `DeepCopy` methods for `TrafficGroupSpec`.

### 3. CRD Manifests (`config/crd/`)

Regenerated CRD manifests via `make manifests` to include `trafficGroup` in the
OpenAPI schema.

### 4. HTTPRoute Reconciler (`pkg/controller/v1beta1/inferenceservice/reconcilers/ingress/httproute_reconciler.go`)

Added `reconcileTrafficGroupHTTPRoute()` which:

- Lists all ISVCs in the same namespace with matching `trafficGroup.name`
- Filters to only predictor-ready members
- Builds a single HTTPRoute with weighted `backendRefs` pointing to each
  member's predictor Service
- Creates or updates the shared HTTPRoute using the group name as the
  HTTPRoute name and hostname prefix

Modified the main `Reconcile()` to route traffic-group ISVCs through this new
path instead of creating individual predictor/top-level HTTPRoutes.

### 5. Model Name Resolution (`pkg/controller/v1beta1/inferenceservice/components/predictor.go`)

When an ISVC is part of a traffic group, the `{{.Name}}` placeholder in runtime
container args (e.g., `--model_name={{.Name}}`) resolves to the traffic group
name instead of the ISVC name. This ensures all group members serve under the
same model identity, so requests to `/v1/models/my-model:predict` succeed
regardless of which backend handles them.

## Demonstration

### Environment

- Kind cluster (v1.35.0) with cert-manager, Gateway API CRDs, and Envoy Gateway
- KServe installed from local repo via kustomize
- Controller image built from this branch and loaded into kind

### InferenceServices Used

```yaml
apiVersion: serving.kserve.io/v1beta1
kind: InferenceService
metadata:
  name: my-model-v1
  namespace: test-traffic-group
spec:
  trafficGroup:
    name: "my-model"
    trafficPercent: 30
  predictor:
    minReplicas: 1
    sklearn:
      storageUri: "gs://kfserving-examples/models/sklearn/1.0/model"
      resources:
        requests:
          cpu: "100m"
          memory: "512Mi"
        limits:
          cpu: "500m"
          memory: "1Gi"
---
apiVersion: serving.kserve.io/v1beta1
kind: InferenceService
metadata:
  name: my-model-v2
  namespace: test-traffic-group
spec:
  trafficGroup:
    name: "my-model"
    trafficPercent: 45
  predictor:
    minReplicas: 1
    sklearn:
      storageUri: "gs://kfserving-examples/models/sklearn/1.0/model"
      resources:
        requests:
          cpu: "100m"
          memory: "512Mi"
        limits:
          cpu: "500m"
          memory: "1Gi"
```

### Results

**Each ISVC owns its own Deployment and Service independently:**
```
DEPLOYMENTS:
  my-model-v1-predictor   1/1 Ready
  my-model-v2-predictor   1/1 Ready

SERVICES:
  my-model-v1-predictor   ClusterIP
  my-model-v2-predictor   ClusterIP
```

**A single shared HTTPRoute is created with weighted backends:**
```
HTTPROUTES:
  my-model   ["my-model-test-traffic-group.example.com"]
```

```json
[
  {
    "kind": "Service",
    "name": "my-model-v1-predictor",
    "weight": 30
  },
  {
    "kind": "Service",
    "name": "my-model-v2-predictor",
    "weight": 45
  }
]
```

**Gateway API normalizes the weights** — 30/(30+45) = 40%, 45/(30+45) = 60%.

**Both model servers register as `my-model`:**
```
v1 args: ["--model_name=my-model", "--model_dir=/mnt/models", "--http_port=8080"]
v2 args: ["--model_name=my-model", "--model_dir=/mnt/models", "--http_port=8080"]
```

**Traffic distribution verified** (50 requests):
```
v1 (weight 30): 20 hits (40%)
v2 (weight 45): 30 hits (60%)
Expected:       v1 ~40%, v2 ~60%
```

All 50 requests returned 200 OK via `/v1/models/my-model:predict`. The end user
has no awareness of the underlying ISVC split.

## Remaining Work

### 1. Controller watch for group membership changes

Currently, when ISVC-v2 joins the traffic group, its reconciliation updates the
shared HTTPRoute. But if ISVC-v1 is deleted, only v1's reconciler fires — it
doesn't trigger v2's reconciler to update the shared HTTPRoute. The controller
needs a watch or index that re-reconciles all group members when any member
changes.

### 2. HTTPRoute ownership

A Gateway API HTTPRoute can only have one controller owner reference. Currently
the first ISVC to create the shared HTTPRoute becomes its owner. If that ISVC is
deleted, the HTTPRoute is garbage collected even though other group members still
need it. Options:

- Use a separate owner (e.g., a ConfigMap or label-based finalizer)
- Create the HTTPRoute without an owner and manage cleanup explicitly
- **Finalizer-based ownership transfer** (preferred): Add a finalizer to every
  traffic group member. When the owning ISVC is deleted, the finalizer blocks
  garbage collection, giving the controller time to patch the HTTPRoute's
  `ownerReferences` to point to another remaining group member, update the
  backends to remove the deleted member, and then remove the finalizer to allow
  deletion to proceed. If no members remain, the finalizer deletes the HTTPRoute
  explicitly. The race window is minimal since the finalizer prevents the ISVC
  from being fully deleted until the transfer is complete.

### 3. Validation

- Validate that `trafficPercent` is > 0
- Warn or reject if only one ISVC exists in a traffic group (no splitting needed)
- Validate that ISVCs in the same group are in the same namespace
- Consider whether `trafficGroup` and `canaryTrafficPercent` should be mutually
  exclusive

### 4. ISVC status for group members

- Report the shared URL (`my-model-...`) as the ISVC's URL instead of the
  individual ISVC URL
- Add a condition or annotation indicating traffic group membership and current
  weight

### 5. Promotion and rollback workflow

Define the user workflow for completing a canary rollout:

- **Promote v2**: Set v2's `trafficPercent` to 100 and delete v1's ISVC
- **Rollback**: Delete v2's ISVC (v1's reconciler updates the HTTPRoute to
  remove v2's backend)
- **Gradual rollout**: Update `trafficPercent` values over time (e.g., 90/10 →
  70/30 → 0/100)

### 6. Support for more than two versions

The implementation already supports N-way splits (not just two). This should be
documented and tested with three or more ISVCs in a group.

### 7. OpenShift Route support

For ODH/RHOAI, the same weighted traffic splitting needs to work with OpenShift
Routes (`alternateBackends`) instead of Gateway API HTTPRoutes. This would be
implemented in odh-model-controller.

### 8. Transformer support

If a traffic group member has a transformer, the shared HTTPRoute should route to
the transformer Service rather than the predictor Service directly.
