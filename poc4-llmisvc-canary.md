# POC 4: Embedded Traffic Splits for LLMInferenceService (Option 4)

## Overview

This POC adds a `splits` field to the existing `router.route` spec on
LLMInferenceService, allowing the stable llmisvc's HTTPRoute to distribute
traffic across multiple llmisvc instances by weight. Each rule in the
HTTPRoute preserves its original backend kind (InferencePool or Service),
path matches, URL rewrites, and timeouts — only the backends are augmented
with weighted entries.

## What Was Implemented

### 1. API Types (`pkg/apis/serving/v1alpha2/llm_inference_service_types.go`)

Added `Splits` field to `GatewayRoutesSpec` and a new `TrafficSplit` struct:

```go
type GatewayRoutesSpec struct {
    HTTP   *HTTPRouteSpec `json:"http,omitempty"`
    Splits []TrafficSplit `json:"splits,omitempty"`
}

type TrafficSplit struct {
    Ref    *corev1.LocalObjectReference `json:"ref,omitempty"`   // nil = self
    Weight int32                        `json:"weight"`
}
```

### 2. Router Reconciler (`pkg/controller/v1alpha2/llmisvc/router.go`)

Added `applyTrafficSplits()` which operates **per-rule**:

- For each rule in the HTTPRoute, examines the existing backendRef to
  determine the backend kind (InferencePool or Service)
- Derives the canary's resource name using the same suffix
  (`-inference-pool` for InferencePool, `-kserve-workload-svc` for Service)
- Preserves the kind, group, and port from the original backendRef
- Runs after config merge and migration logic in `expectedHTTPRoute()`,
  operating on the fully resolved HTTPRoute spec

## How the HTTPRoute Is Generated

The HTTPRoute for an llmisvc is assembled through a multi-step pipeline:

1. **LLMInferenceServiceConfig templates** (e.g., `kserve-config-llm-router-route`)
   define the HTTPRoute rules with Go templates. These templates produce
   multiple rules with different path matches, URL rewrites, and backend types.

2. **Config merge** (`config_merge.go`) resolves templates and merges configs
   into the llmisvc's effective spec. It also swaps backend refs based on
   whether a scheduler is configured:
   - With scheduler: backends point to **InferencePool** resources
   - Without scheduler: backends point to **Service** resources

3. **`expectedHTTPRoute`** (`router.go`) copies the resolved spec into the
   HTTPRoute object.

4. **`applyTrafficSplits`** runs after the spec is copied, adding weighted
   backends per-rule while preserving the existing backend kind.

5. **Migration logic** may further swap InferencePool API groups
   (v1alpha2 → v1).

A typical llmisvc with `scheduler: {}` produces 4 rules:

| Rule | Path Match | Backend Kind | Purpose |
|------|-----------|-------------|---------|
| 1 | `/{ns}/{name}/v1/completions` | InferencePool | Completions → Scheduler/EPP |
| 2 | `/{ns}/{name}/v1/chat/completions` | InferencePool | Chat → Scheduler/EPP |
| 3 | `/{ns}/{name}/v1/responses` | InferencePool | Responses → Scheduler/EPP |
| 4 | `/{ns}/{name}` (catch-all) | Service | Health, model info → direct |

## Demonstration

### Environment

Kind cluster with:
- cert-manager v1.17.0
- Gateway API CRDs v1.4.1
- Envoy Gateway v1.6.3 with AI Gateway extension
- LeaderWorkerSet
- Gateway API Inference Extension CRDs (InferencePool v1 + v1alpha2)
- llmisvc controller built from this branch

### Input: Stable LLMInferenceService (v1)

Created using the upstream getting-started example (`facebook/opt-125m`
on CPU via vLLM):

```yaml
apiVersion: serving.kserve.io/v1alpha2
kind: LLMInferenceService
metadata:
  name: facebook-opt-125m-single
  namespace: llm-demo
spec:
  model:
    name: facebook/opt-125m
    uri: "hf://facebook/opt-125m"
  replicas: 1
  router:
    gateway: {}
    route: {}
    scheduler: {}
  template:
    containers:
    - name: main
      image: quay.io/pierdipi/vllm-cpu:latest
      env:
      - name: VLLM_LOGGING_LEVEL
        value: DEBUG
      resources:
        limits:
          cpu: "1"
          memory: "10Gi"
        requests:
          cpu: "100m"
          memory: "8Gi"
      securityContext:
        runAsNonRoot: false
```

### Input: Canary LLMInferenceService (v2)

Same spec under a different name:

```yaml
apiVersion: serving.kserve.io/v1alpha2
kind: LLMInferenceService
metadata:
  name: facebook-opt-125m-canary
  namespace: llm-demo
spec:
  model:
    name: facebook/opt-125m
    uri: "hf://facebook/opt-125m"
  replicas: 1
  router:
    gateway: {}
    route: {}
    scheduler: {}
  template:
    containers:
    - name: main
      image: quay.io/pierdipi/vllm-cpu:latest
      env:
      - name: VLLM_LOGGING_LEVEL
        value: DEBUG
      resources:
        limits:
          cpu: "1"
          memory: "10Gi"
        requests:
          cpu: "100m"
          memory: "8Gi"
      securityContext:
        runAsNonRoot: false
```

### Input: Adding splits to v1

```bash
kubectl patch llmisvc facebook-opt-125m-single -n llm-demo --type=merge -p '{
  "spec": {
    "router": {
      "route": {
        "splits": [
          {"weight": 80},
          {"ref": {"name": "facebook-opt-125m-canary"}, "weight": 20}
        ]
      }
    }
  }
}'
```

### Output: HTTPRoute per-rule backends

**Rules 1-3 (InferencePool backends — routed through Scheduler/EPP):**

```json
{
  "match": "/llm-demo/facebook-opt-125m-single/v1/completions",
  "backends": [
    { "kind": "InferencePool", "name": "facebook-opt-125m-single-inference-pool", "weight": 80 },
    { "kind": "InferencePool", "name": "facebook-opt-125m-canary-inference-pool", "weight": 20 }
  ]
}
{
  "match": "/llm-demo/facebook-opt-125m-single/v1/chat/completions",
  "backends": [
    { "kind": "InferencePool", "name": "facebook-opt-125m-single-inference-pool", "weight": 80 },
    { "kind": "InferencePool", "name": "facebook-opt-125m-canary-inference-pool", "weight": 20 }
  ]
}
{
  "match": "/llm-demo/facebook-opt-125m-single/v1/responses",
  "backends": [
    { "kind": "InferencePool", "name": "facebook-opt-125m-single-inference-pool", "weight": 80 },
    { "kind": "InferencePool", "name": "facebook-opt-125m-canary-inference-pool", "weight": 20 }
  ]
}
```

**Rule 4 (Service backend — direct, no Scheduler):**

```json
{
  "match": "/llm-demo/facebook-opt-125m-single",
  "backends": [
    { "kind": "Service", "name": "facebook-opt-125m-single-kserve-workload-svc", "weight": 80 },
    { "kind": "Service", "name": "facebook-opt-125m-canary-kserve-workload-svc", "weight": 20 }
  ]
}
```

### Output: HTTPRoute status

```
Accepted:      True
ResolvedRefs:  True
```

The Gateway controller accepted the HTTPRoute and resolved all backend
references (both InferencePool and Service backends).

### Output: Cluster resources

```
LLMINFERENCESERVICES:
  facebook-opt-125m-single   Ready: True
  facebook-opt-125m-canary   Ready: False (MinimumReplicasUnavailable — vLLM startup)

PODS:
  facebook-opt-125m-single-kserve-...                   1/1 Running
  facebook-opt-125m-single-kserve-router-scheduler-...  2/2 Running
  facebook-opt-125m-canary-kserve-...                   0/1 Pending (model loading)
  facebook-opt-125m-canary-kserve-router-scheduler-...  1/2 Running

SERVICES:
  facebook-opt-125m-single-kserve-workload-svc   8000/TCP
  facebook-opt-125m-single-epp-service           9002,9003,9090,5557/TCP
  facebook-opt-125m-canary-kserve-workload-svc   8000/TCP
  facebook-opt-125m-canary-epp-service           9002,9003,9090,5557/TCP

INFERENCEPOOLS:
  facebook-opt-125m-single-inference-pool
  facebook-opt-125m-canary-inference-pool
```

Each llmisvc independently owns its Deployment, Service, InferencePool,
and Scheduler. The splits only affect the stable llmisvc's HTTPRoute.

### Output: Full HTTPRoute YAML

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  annotations:
    serving.kserve.io/inference-pool-migrated: v1
  labels:
    app.kubernetes.io/component: llminferenceservice-router
    app.kubernetes.io/name: facebook-opt-125m-single
    app.kubernetes.io/part-of: llminferenceservice
  name: facebook-opt-125m-single-kserve-route
  namespace: llm-demo
  ownerReferences:
  - apiVersion: serving.kserve.io/v1alpha2
    blockOwnerDeletion: true
    controller: true
    kind: LLMInferenceService
    name: facebook-opt-125m-single
spec:
  parentRefs:
  - group: gateway.networking.k8s.io
    kind: Gateway
    name: kserve-ingress-gateway
    namespace: kserve
  rules:
  - backendRefs:
    - group: inference.networking.k8s.io
      kind: InferencePool
      name: facebook-opt-125m-single-inference-pool
      port: 8000
      weight: 80
    - group: inference.networking.k8s.io
      kind: InferencePool
      name: facebook-opt-125m-canary-inference-pool
      port: 8000
      weight: 20
    filters:
    - type: URLRewrite
      urlRewrite:
        path:
          replacePrefixMatch: /v1/completions
          type: ReplacePrefixMatch
    matches:
    - path:
        type: PathPrefix
        value: /llm-demo/facebook-opt-125m-single/v1/completions
    timeouts:
      backendRequest: 0s
      request: 0s
  - backendRefs:
    - group: inference.networking.k8s.io
      kind: InferencePool
      name: facebook-opt-125m-single-inference-pool
      port: 8000
      weight: 80
    - group: inference.networking.k8s.io
      kind: InferencePool
      name: facebook-opt-125m-canary-inference-pool
      port: 8000
      weight: 20
    filters:
    - type: URLRewrite
      urlRewrite:
        path:
          replacePrefixMatch: /v1/chat/completions
          type: ReplacePrefixMatch
    matches:
    - path:
        type: PathPrefix
        value: /llm-demo/facebook-opt-125m-single/v1/chat/completions
    timeouts:
      backendRequest: 0s
      request: 0s
  - backendRefs:
    - group: inference.networking.k8s.io
      kind: InferencePool
      name: facebook-opt-125m-single-inference-pool
      port: 8000
      weight: 80
    - group: inference.networking.k8s.io
      kind: InferencePool
      name: facebook-opt-125m-canary-inference-pool
      port: 8000
      weight: 20
    filters:
    - type: URLRewrite
      urlRewrite:
        path:
          replacePrefixMatch: /v1/responses
          type: ReplacePrefixMatch
    matches:
    - path:
        type: PathPrefix
        value: /llm-demo/facebook-opt-125m-single/v1/responses
    timeouts:
      backendRequest: 0s
      request: 0s
  - backendRefs:
    - group: ""
      kind: Service
      name: facebook-opt-125m-single-kserve-workload-svc
      port: 8000
      weight: 80
    - group: ""
      kind: Service
      name: facebook-opt-125m-canary-kserve-workload-svc
      port: 8000
      weight: 20
    filters:
    - type: URLRewrite
      urlRewrite:
        path:
          replacePrefixMatch: /
          type: ReplacePrefixMatch
    matches:
    - path:
        type: PathPrefix
        value: /llm-demo/facebook-opt-125m-single
    timeouts:
      backendRequest: 0s
      request: 0s
status:
  parents:
  - conditions:
    - message: Route is accepted
      reason: Accepted
      status: "True"
      type: Accepted
    - message: Resolved all the Object references for the Route
      reason: ResolvedRefs
      status: "True"
      type: ResolvedRefs
    controllerName: gateway.envoyproxy.io/gatewayclass-controller
    parentRef:
      group: gateway.networking.k8s.io
      kind: Gateway
      name: kserve-ingress-gateway
      namespace: kserve
```

## Remaining Work

### 1. End-to-end traffic verification

The HTTPRoute is accepted and refs are resolved, but actual request routing
through the Scheduler/EPP with weighted traffic has not been tested with
curl. The canary pod was still starting at the time of this POC (vLLM CPU
startup takes 10+ minutes on kind).

### 2. InferencePool existence verification

The canary's InferencePool must exist before it can be referenced. The
controller currently adds the backend optimistically. It should verify the
pool exists or requeue until the canary llmisvc has been reconciled.

### 3. Route suppression for canary

The canary llmisvc currently gets its own HTTPRoute
(`facebook-opt-125m-canary-kserve-route`). Whether this should be suppressed
(traffic only via the stable's route) or kept (direct testing access) should
be configurable.

### 4. Model name consistency

Both llmisvcs set `model.name: facebook/opt-125m` — the same model identity.
This is required for transparent canary. If model names differ, requests
routed to the canary may fail or return unexpected results.

### 5. Session affinity

With per-request weighted routing, a user's requests may alternate between
v1 and v2 across turns in a conversation. Gateway API's `sessionPersistence`
(experimental, on HTTPRouteRule) could pin users to a version. This is a
follow-up concern, not specific to this POC.
