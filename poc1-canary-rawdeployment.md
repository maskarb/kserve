# POC 1: Canary Traffic Splitting for RawDeployment Mode

## Overview

This POC demonstrates that KServe's existing `canaryTrafficPercent` API field —
which currently only works in Knative mode — can be extended to RawDeployment
mode using Gateway API HTTPRoute weighted backends.

## What Was Implemented

Four files were modified (90 lines added):

### 1. Deployment Reconciler (`pkg/controller/v1beta1/inferenceservice/reconcilers/deployment/deployment_reconciler.go`)

When `canaryTrafficPercent > 0`, a second Deployment is created with a `-canary`
suffix. Both the default and canary Deployments are reconciled through the
existing lifecycle (create/update/delete).

### 2. Service Reconciler (`pkg/controller/v1beta1/inferenceservice/reconcilers/service/service_reconciler.go`)

A matching `-canary` Service is created alongside the canary Deployment, with a
selector targeting the canary pods via the `app` label.

### 3. HTTPRoute Reconciler (`pkg/controller/v1beta1/inferenceservice/reconcilers/ingress/httproute_reconciler.go`)

A `WeightedBackend` struct and `createWeightedHTTPRouteRule()` function were
added. Both the predictor-level and top-level HTTPRoutes use weighted
`backendRefs` when canary is configured:

- Default service gets weight `100 - canaryTrafficPercent`
- Canary service gets weight `canaryTrafficPercent`

When canary is not configured, the existing single-backend behavior is preserved.

### 4. Status Propagation (`pkg/controller/v1beta1/inferenceservice/components/predictor.go`)

The canary Deployment is currently filtered out of the status propagation list so
that only the default Deployment's status drives the `PredictorReady` condition.
Without this fix, the multi-deployment status aggregation (designed for
multi-node head/worker) incorrectly sets the condition to `Unknown`.

**Note:** This is a temporary workaround. The proper solution is to add dedicated
canary status conditions (see "Canary status reporting" in Remaining Work).

## Demonstration

### Environment

- Kind cluster (v1.35.0) with cert-manager, Gateway API CRDs, and Envoy Gateway
- KServe installed from local repo via kustomize (`config/overlays/standalone/kserve`)
- Controller image built from this branch and loaded into kind

### InferenceService Used

```yaml
apiVersion: serving.kserve.io/v1beta1
kind: InferenceService
metadata:
  name: sklearn-canary
  namespace: test-canary
spec:
  predictor:
    canaryTrafficPercent: 20
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

**Deployments** — two created, both Ready:
```
NAME                              READY
sklearn-canary-predictor          1/1
sklearn-canary-predictor-canary   1/1
```

**Services** — two created:
```
NAME                              TYPE        CLUSTER-IP
sklearn-canary-predictor          ClusterIP   10.96.117.85
sklearn-canary-predictor-canary   ClusterIP   10.96.190.93
```

**HTTPRoutes** — weighted backends on both predictor and top-level routes:
```json
[
  {
    "kind": "Service",
    "name": "sklearn-canary-predictor",
    "port": 80,
    "weight": 80
  },
  {
    "kind": "Service",
    "name": "sklearn-canary-predictor-canary",
    "port": 80,
    "weight": 20
  }
]
```

**ISVC Status** — fully Ready:
```
NAME             URL                                             READY
sklearn-canary   http://sklearn-canary-test-canary.example.com   True
```

## Current Limitation

Both the default and canary Deployments run the **same model spec**. Setting
`canaryTrafficPercent` creates the traffic splitting infrastructure but there
is no mechanism to specify a different model version for the canary.

## Remaining Work to Make This Feature Complete

### 1. Canary-on-update semantics

The core missing piece. When the user updates the predictor spec (e.g., changes
`storageUri` to a new model version) AND `canaryTrafficPercent > 0`:

- The **existing** Deployment should be kept as-is (old model = default)
- A **new** canary Deployment should be created with the updated spec
- The HTTPRoute should split traffic between old and new

This requires changes to `Reconcile()` in the deployment reconciler: when the
check result is `Update` and canary is configured, skip updating the default
Deployment and instead create a canary Deployment with the desired (new) spec.

If the spec hasn't changed, `canaryTrafficPercent` should be a no-op.

### 2. Canary promotion and rollback

- **Promote**: User sets `canaryTrafficPercent: 100`. Controller updates the
  default Deployment to the canary spec and deletes the canary
  Deployment/Service. The user can then remove `canaryTrafficPercent` from the
  spec; since the default now matches the desired spec, it becomes a no-op.
- **Rollback**: User sets `canaryTrafficPercent: 0` or removes it. Controller
  deletes the canary Deployment/Service and restores full traffic to the
  unchanged default Deployment.
- **Gradual rollout**: User increases `canaryTrafficPercent` from 20 to 50 to
  100. Controller updates HTTPRoute weights accordingly.

### 3. Canary status reporting

Replace the current workaround (filtering canary deployments from status
propagation) with dedicated canary conditions:

- `CanaryPredictorReady` — reflects the canary Deployment's health, independent
  of the default Deployment's `PredictorReady` condition.
- `CanaryIngressReady` — reflects whether the weighted HTTPRoute is accepted by
  the gateway controller.
- The top-level `Ready` condition should require both `PredictorReady` and
  `CanaryPredictorReady` when canary is active.
- Surface canary metadata in `.status.components.predictor` (e.g., canary
  traffic percentage, canary URL for direct testing).

### 4. ~~Canary cleanup on ISVC deletion~~ (verified)

Canary Deployments, Services, and HTTPRoutes are properly garbage collected when
the ISVC is deleted. Owner references handle this automatically — verified on
the kind cluster.

### 5. Transformer and Explainer support

The current implementation only handles the predictor component. Canary support
for transformers and explainers would require similar changes in their respective
reconcilers.

### 6. Unit and integration tests

- Unit tests for `createWeightedHTTPRouteRule` and the canary deployment/service
  creation logic.
- Integration tests (envtest) verifying the full reconciliation loop with canary
  configured.
- E2E test deploying two model versions with traffic splitting.

### 7. OpenShift Route support

For ODH/RHOAI, the same weighted traffic splitting needs to work with OpenShift
Routes (`alternateBackends`) instead of Gateway API HTTPRoutes. This would be
implemented in odh-model-controller.
