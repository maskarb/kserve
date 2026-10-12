# Confidential LLM serving on bare-metal OpenShift nodes

`kserve-config-llm-confidential-bare-metal` is an opt-in RHOAI preset for
single-node NVIDIA GPU serving with OSC CoCo, Trustee/CDH, and a raw block PVC
encrypted inside the TEE. It targets the `kata-cc-nvidia-gpu` runtime on
bare-metal nodes. Peer-pod and cloud deployments require a separate preset.

The RHOAI KServe overlay installs the preset in the applications namespace.
The KServe module prefixes shipped presets with the release version. Find the
installed name and use that exact name in `spec.baseRefs`:

```sh
oc get llminferenceserviceconfigs -n <RHOAI_APPLICATIONS_NAMESPACE> -o name | rg 'kserve-config-llm-confidential-bare-metal$'
```

## Prepare the cluster and namespace

Install and configure OSC with the NVIDIA GPU operator, the confidential
runtime class, and Trustee before using this sample. Prepare and measure your
`initdata.toml`, configure the Trustee attestation policy and reference values,
and register the model and LUKS keys in KBS. These operations belong to the
cluster and Trustee administrators.

The namespace bundle creates `coco-cc-storage`, a namespace-scoped RoleBinding
to the privileged SCC, and a `cc-storage-link` ConfigMap containing both helper
scripts. An administrator must apply this bundle to an approved confidential
serving namespace. Keep the service account restricted to workloads using the
confidential runtime. For another namespace, change both `namespace.yaml` and
the namespace in `namespace/kustomization.yaml`.

From the repository root:

```sh
oc apply -k docs/samples/llmisvc/confidential-bare-metal/namespace
```

Create the download credentials in that namespace. The preset reads them using
`envFrom`, so use the storage initializer's environment variable names. For a
Hugging Face model:

```sh
oc create secret generic model-download-credentials -n confidential-model-serving --from-literal=HF_TOKEN=<HF_TOKEN>
```

For S3, populate this secret with the `AWS_ACCESS_KEY_ID`,
`AWS_SECRET_ACCESS_KEY`, `AWS_ENDPOINT_URL`, `AWS_DEFAULT_REGION`, `S3_ENDPOINT`,
and `S3_USE_HTTPS` values required by your endpoint.

Create `luks-sealed` with a `key` entry containing the OSC sealed pointer to
`kbs:///default/luks-password/key`. The actual passphrase stays in KBS. For the
OSC 1.13 unsigned vault envelope used by this sample:

```sh
LUKS_POINTER=$(python3 -c 'import base64,json; payload={"version":"0.1.0","type":"vault","name":"kbs:///default/luks-password/key","provider":"kbs","provider_settings":{},"annotations":{}}; print("sealed.fakejwsheader."+base64.urlsafe_b64encode(json.dumps(payload,separators=(",",":")).encode()).decode()+".fakesignature")')
oc create secret generic luks-sealed -n confidential-model-serving --from-literal=key="$LUKS_POINTER"
```

For a different LUKS key, change both the pointer and the `LUKS_RESOURCE_ID`
environment variable on the `format-disk` init container in the service's
`spec.template.initContainers`. The latter supplies the CDH fallback when OSC
has not yet unsealed `PASS`. The passphrase must be a nonempty single-line text
value. Override `PASS.valueFrom.secretKeyRef` when using another sealed secret.

## Prepare the model service

Copy and edit `pvc.yaml` and `llm-inference-service.yaml` before applying them.
The preset derives the PVC name as `<service-name>-encrypted-model`; the sample
therefore uses `confidential-llm-encrypted-model`. Use a dedicated empty block
PVC for each service and choose a StorageClass that supports `volumeMode: Block`.
The helper formats new devices inside the TEE. It fails on an existing LUKS
volume that cannot be unlocked, leaving the device intact. Reinitializing or
rotating a volume key is a separate administrative operation.

Set the installed versioned preset name in `baseRefs`, replace the model URI
and KBS model key reference, and provide the encoded initdata annotation:

```sh
python3 -c 'import base64,gzip,pathlib; print(base64.b64encode(gzip.compress(pathlib.Path("initdata.toml").read_bytes(),mtime=0)).decode())'
```

Model download arguments and `CONFIDENTIAL_RESOURCE_ID` are rendered from
`spec.model`; each service specifies them once. The initializer rejects a
missing confidential-enabled flag or model key reference before downloading.
The preset keeps automatic storage initializer injection disabled because its
wrapper needs to link `/mnt/models` to the helper's LUKS mount first.

The preset obtains vLLM, storage initializer, and OSC helper images from the
RHOAI overlay's image parameters. The module supports
`RELATED_IMAGE_OSC_STORAGE_HELPER_IMAGE` for the helper. Use final registry
paths reachable by the guest and allow those images in your Trustee image
verification policy. Validate your RHOAI/OSC image combination on CoCo hardware
before rollout; this sample does not establish product support for a release.
The CUDA library environment variables and `VLLM_USE_V2_MODEL_RUNNER=0` are
compatibility workarounds inherited from the initial bare-metal deployment.

Adjust resources and any `VLLM_ADDITIONAL_ARGS` for your model. The sample
reserves 58 GiB for a 56 GiB guest, allowing overhead in line with
[OSC memory sizing guidance](https://docs.redhat.com/en/documentation/openshift_sandboxed_containers/1.13/html/deploying_confidential_containers_on_bare-metal_servers/configure-cc-overview_metal-cc).
Scratch volumes use bounded tmpfs; model weights, download caches, and
decryption scratch files use the encrypted PVC. Size the PVC for both downloaded
encrypted files and decrypted weights.

The initial scope is one replica per dedicated PVC. The preset sets
`maxSurge: 0` and `maxUnavailable: 1`, allowing downtime while replacing the pod.
Autoscaling, multiple replicas sharing a block device, multi-node serving, and
disaggregated prefill/decode need separate storage lifecycle validation.

To reference an existing PVC, override only the named volume on your service:

```yaml
spec:
  template:
    volumes:
      - name: luks-block
        persistentVolumeClaim:
          claimName: my-model-block-pvc
```

## Verify the deployment

After applying the edited PVC and service manifests, check the effective pod
and the `status.appliedConfigs` entries on the service. The init sequence is
`format-disk` (restartable sidecar), `check-ready`, `storage-initializer`, then
`main`. The base preset supplies the vLLM launcher, probes, and TLS configuration.

```sh
oc get llminferenceservice,pod,pvc,httproute -n confidential-model-serving
oc logs -n confidential-model-serving <POD> -c format-disk
oc logs -n confidential-model-serving <POD> -c storage-initializer
```

Validate initial download/decryption, inference, pod replacement using the same
PVC, and a deliberately incorrect key leaving the existing volume intact.
Confirm successful attestation through Trustee/CDH. When helper scripts or
image versions change, review the measured guest policy and compatibility with
the pinned preset before restarting workloads.
