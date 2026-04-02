# POC 3: InferenceTrafficSplit CRD

## Overview

This POC introduces a new `InferenceTrafficSplit` CRD (v1alpha1) that defines
weighted traffic splitting across multiple independent InferenceServices. Unlike
POC 2's `trafficGroup` approach, the InferenceTrafficSplit is a standalone
resource that owns the shared HTTPRoute, cleanly solving the ownership and
garbage collection problems.

ISVCs remain plain — no spec changes needed. The InferenceTrafficSplit
references them by name and assigns traffic weights.

## What Was Implemented

### 1. CRD Types (`pkg/apis/serving/v1alpha1/inference_traffic_split.go`)

```go
type InferenceTrafficSplitSpec struct {
    Backends []TrafficSplitBackend `json:"backends"`
}

type TrafficSplitBackend struct {
    InferenceServiceRef string `json:"inferenceServiceRef"`
    Weight              int32  `json:"weight"`
}
```

Status includes URL, Address, and standard Conditions.

### 2. Controller (`pkg/controller/v1alpha1/inferencetrafficsplit/controller.go`)

The InferenceTrafficSplitReconciler:

- Lists referenced ISVCs and builds weighted `backendRefs` for ready members
- Creates/updates a shared HTTPRoute owned by the InferenceTrafficSplit
- Labels referenced ISVCs with `serving.kserve.io/traffic-split: <its-name>` so
  the ISVC ingress reconciler knows to skip individual HTTPRoute creation
- Uses a finalizer to remove labels from ISVCs when the ITS is deleted
- Watches InferenceService changes via `EnqueueRequestsFromMapFunc` so that
  when a referenced ISVC becomes ready, the ITS is re-reconciled

### 3. ISVC HTTPRoute suppression (`pkg/controller/v1beta1/inferenceservice/reconcilers/ingress/httproute_reconciler.go`)

When an ISVC has the `serving.kserve.io/traffic-split` label, the ISVC ingress
reconciler skips creating individual predictor and top-level HTTPRoutes. Traffic
is only routable through the ITS's shared HTTPRoute, preventing users from
bypassing the traffic split.

### 4. RBAC and CRD manifests

- Generated CRD: `config/crd/full/serving.kserve.io_inferencetrafficsplits.yaml`
- Updated `config/rbac/role.yaml` with `inferencetrafficsplits` permissions
- Controller registered in `cmd/manager/main.go`

## Demonstration

### Environment

- Kind cluster (v1.35.0) with cert-manager, Gateway API CRDs, and Envoy Gateway
- KServe installed from local repo via kustomize
- Controller image built from this branch and loaded into kind

### Resources Created

```yaml
apiVersion: serving.kserve.io/v1beta1
kind: InferenceService
metadata:
  name: sklearn-v1
  namespace: test-its
spec:
  predictor:
    minReplicas: 1
    sklearn:
      storageUri: "gs://kfserving-examples/models/sklearn/1.0/model"
      resources:
        requests: { cpu: "100m", memory: "512Mi" }
        limits: { cpu: "500m", memory: "1Gi" }
---
apiVersion: serving.kserve.io/v1beta1
kind: InferenceService
metadata:
  name: sklearn-v2
  namespace: test-its
spec:
  predictor:
    minReplicas: 1
    sklearn:
      storageUri: "gs://kfserving-examples/models/sklearn/1.0/model"
      resources:
        requests: { cpu: "100m", memory: "512Mi" }
        limits: { cpu: "500m", memory: "1Gi" }
---
apiVersion: serving.kserve.io/v1alpha1
kind: InferenceTrafficSplit
metadata:
  name: sklearn-prod
  namespace: test-its
spec:
  backends:
  - inferenceServiceRef: sklearn-v1
    weight: 80
  - inferenceServiceRef: sklearn-v2
    weight: 20
```

Note: the ISVCs are completely standard — no traffic-related fields.

### Results

**Only one HTTPRoute** — individual ISVC routes are suppressed:
```
NAME           HOSTNAMES
sklearn-prod   ["sklearn-prod.example.com"]
```

**ISVCs labeled automatically** by the ITS controller:
```
NAME         TRAFFIC-SPLIT    READY
sklearn-v1   sklearn-prod     True
sklearn-v2   sklearn-prod     True
```

**HTTPRoute backends with correct weights:**
```json
[
  { "name": "sklearn-v1-predictor", "weight": 80 },
  { "name": "sklearn-v2-predictor", "weight": 20 }
]
```

**InferenceTrafficSplit status:**
```
NAME           URL                               READY
sklearn-prod   http://sklearn-prod.example.com   True
```

## Advantages Over POC 2

| Concern | POC 2 (trafficGroup) | POC 3 (InferenceTrafficSplit) |
|---|---|---|
| HTTPRoute ownership | Arbitrary ISVC owns it — deletion kills all backends | ITS owns it — clean lifecycle |
| ISVC spec changes | Required (`trafficGroup` field) | None — plain ISVCs |
| Garbage collection | Broken without finalizer workaround | Works automatically via owner refs |
| Group membership watch | Requires listing ISVCs by field | Built-in via `EnqueueRequestsFromMapFunc` |
| Route suppression | Not implemented | Label-based — individual routes skipped |
| Status | Spread across ISVCs | Centralized on the ITS resource |

## Remaining Work

### 1. Model name consistency

Same issue as POC 2: each ISVC's model server registers with its own name
(`sklearn-v1`, `sklearn-v2`). Requests through the shared endpoint hit 404 on
the wrong backend. Options:

- Add a `modelName` override field to `TrafficSplitBackend`
- Use the ITS name as the model name (requires the ISVC reconciler to check
  for the traffic-split label and override `--model_name`)
- Add an HTTPRoute `URLRewrite` filter (limited by Gateway API — rewrites
  apply before backend selection, not per-backend)

### 2. Label cleanup edge cases

- If an ISVC is removed from the ITS spec (not deleted, just removed from
  `backends`), the label should be removed and individual HTTPRoutes restored
- If an ISVC is referenced by multiple ITS resources (should be rejected by
  validation)

### 3. Validation webhook

- Reject `trafficPercent <= 0`
- Reject duplicate `inferenceServiceRef` entries
- Reject references to ISVCs in different namespaces (cross-namespace not
  supported)
- Reject ISVCs already referenced by another InferenceTrafficSplit
- Warn if only one backend (no splitting needed)

### 4. Existing HTTPRoute cleanup

When an ISVC is added to an ITS, any existing individual HTTPRoutes for that
ISVC should be deleted. Currently the label prevents new routes from being
created, but pre-existing routes remain until the ISVC is re-reconciled.

### 5. Status improvements

- Report per-backend status (ready/not ready, current weight)
- Report normalized traffic percentages
- Propagate HTTPRoute conditions to the ITS status

### 6. Transformer support

When a backend ISVC has a transformer, the shared HTTPRoute should point to the
transformer Service instead of the predictor Service.

### 7. OpenShift Route support

For ODH/RHOAI, the same weighted traffic splitting needs to work with OpenShift
Routes (`alternateBackends`) instead of Gateway API HTTPRoutes. This would be
implemented in odh-model-controller.
