//go:build distro

/*
Copyright 2026 The KServe Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package llmisvc_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"knative.dev/pkg/apis"
	"sigs.k8s.io/yaml"

	"github.com/kserve/kserve/pkg/apis/serving/v1alpha2"
	"github.com/kserve/kserve/pkg/controller/v1alpha2/llmisvc"
	kservetesting "github.com/kserve/kserve/pkg/testing"
	"github.com/kserve/kserve/pkg/utils"
)

func loadBareMetalPreset(t *testing.T, path string) *v1alpha2.LLMInferenceServiceConfig {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(kservetesting.ProjectRoot(), path)) //nolint:gosec // Paths are fixed repository preset fixtures.
	require.NoError(t, err)
	var preset v1alpha2.LLMInferenceServiceConfig
	require.NoError(t, yaml.Unmarshal(raw, &preset))
	return &preset
}

func TestConfidentialBareMetalPresetMerge(t *testing.T) {
	base := loadBareMetalPreset(t, "config/llmisvcconfig/config-llm-template.yaml")
	preset := loadBareMetalPreset(t, "config/overlays/odh/llmisvcconfig/config-llm-confidential-bare-metal.yaml")

	for _, uri := range []string{"hf://example/encrypted-model", "s3://models/encrypted-model"} {
		t.Run(uri, func(t *testing.T) {
			service := llmisvc.LLMInferenceServiceSample()
			modelURI, err := apis.ParseURL(uri)
			require.NoError(t, err)
			service.Spec.Model.URI = *modelURI
			service.Spec.Model.Confidential = &v1alpha2.ConfidentialSpec{
				Enabled:    true,
				ResourceId: ptr.To("kbs:///team/model-keys/weights"),
			}
			// Partial container overrides must preserve the sidecar ordering.
			service.Spec.Template = &corev1.PodSpec{
				InitContainers: []corev1.Container{{
					Name: "storage-initializer",
					Env:  []corev1.EnvVar{{Name: "AWS_DEFAULT_REGION", Value: "us-east-1"}},
				}},
			}
			merged, err := llmisvc.MergeSpecs(t.Context(), base.Spec, preset.Spec, service.Spec)
			require.NoError(t, err)
			service.Spec = merged
			rendered, err := llmisvc.ReplaceVariables(service, &v1alpha2.LLMInferenceServiceConfig{Spec: merged}, &llmisvc.Config{EnableTLS: true})
			require.NoError(t, err)

			pod := rendered.Spec.Template
			require.Equal(t, ptr.To("kata-cc-nvidia-gpu"), pod.RuntimeClassName)
			require.Equal(t, "coco-cc-storage", pod.ServiceAccountName)
			require.Equal(t, ptr.To(true), pod.ShareProcessNamespace)
			require.Equal(t, ptr.To(false), rendered.Spec.StorageInitializer.Enabled)
			require.EqualValues(t, 0, rendered.Spec.RolloutStrategy.MaxSurge.IntVal)
			require.EqualValues(t, 1, rendered.Spec.RolloutStrategy.MaxUnavailable.IntVal)
			require.Len(t, pod.InitContainers, 3)
			require.Equal(t, []string{"format-disk", "check-ready", "storage-initializer"}, []string{
				pod.InitContainers[0].Name, pod.InitContainers[1].Name, pod.InitContainers[2].Name,
			})
			require.Equal(t, ptr.To(corev1.ContainerRestartPolicyAlways), pod.InitContainers[0].RestartPolicy)
			initializer := &pod.InitContainers[2]
			require.Equal(t, []string{uri, "/mnt/models"}, initializer.Args)
			env := map[string]string{}
			for _, entry := range initializer.Env {
				env[entry.Name] = entry.Value
			}
			require.Equal(t, "true", env["CONFIDENTIAL_ENABLED"])
			require.Equal(t, "kbs:///team/model-keys/weights", env["CONFIDENTIAL_RESOURCE_ID"])
			require.Equal(t, "us-east-1", env["AWS_DEFAULT_REGION"])
			require.Contains(t, initializer.Command[2], ". /opt/cc-storage/link.sh")

			main := utils.GetContainerWithName(pod, "main")
			require.NotNil(t, main)
			require.Equal(t, "/bin/bash", main.Command[0])
			require.Contains(t, main.Command[2], "--ssl-certfile")
			require.NotNil(t, main.StartupProbe)
			require.Equal(t, corev1.URISchemeHTTPS, main.StartupProbe.HTTPGet.Scheme)
			require.Equal(t, ptr.To(true), main.SecurityContext.Privileged)
			require.Equal(t, ptr.To(false), main.SecurityContext.RunAsNonRoot)
			require.Equal(t, corev1.SeccompProfileTypeUnconfined, main.SecurityContext.SeccompProfile.Type)
			// No container may mount over the path that link.sh replaces.
			for _, container := range append(pod.Containers, pod.InitContainers...) {
				for _, mount := range container.VolumeMounts {
					require.NotEqual(t, "/mnt/models", mount.MountPath)
					if mount.MountPath == "/dev/shm" {
						require.Equal(t, "dshm", mount.Name)
					}
				}
			}
			for _, volume := range pod.Volumes {
				switch volume.Name {
				case "luks-block":
					require.Equal(t, service.Name+"-encrypted-model", volume.PersistentVolumeClaim.ClaimName)
				case "home", "model-cache", "tmp-dir", "dshm":
					require.Equal(t, corev1.StorageMediumMemory, volume.EmptyDir.Medium)
					require.Equal(t, "1Gi", volume.EmptyDir.SizeLimit.String())
				}
			}
		})
	}
	// Selecting the confidential overlay must not change the default preset.
	require.Equal(t, ptr.To(false), base.Spec.Template.Containers[0].SecurityContext.AllowPrivilegeEscalation)
	require.Equal(t, ptr.To(true), base.Spec.Template.Containers[0].SecurityContext.RunAsNonRoot)
}

func TestConfidentialBareMetalCustomPVC(t *testing.T) {
	preset := loadBareMetalPreset(t, "config/overlays/odh/llmisvcconfig/config-llm-confidential-bare-metal.yaml")
	service := llmisvc.LLMInferenceServiceSample()
	service.Spec.Template = &corev1.PodSpec{Volumes: []corev1.Volume{{
		Name: "luks-block",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "existing-encrypted-pvc"},
		},
	}}}
	merged, err := llmisvc.MergeSpecs(t.Context(), preset.Spec, service.Spec)
	require.NoError(t, err)
	service.Spec = merged
	rendered, err := llmisvc.ReplaceVariables(service, &v1alpha2.LLMInferenceServiceConfig{Spec: merged}, nil)
	require.NoError(t, err, "missing confidential settings should render and fail closed in the initializer")
	for _, volume := range rendered.Spec.Template.Volumes {
		if volume.Name == "luks-block" {
			require.Equal(t, "existing-encrypted-pvc", volume.PersistentVolumeClaim.ClaimName)
			return
		}
	}
	t.Fatal("luks-block volume missing")
}

func TestConfidentialBareMetalPreservesExistingVolume(t *testing.T) {
	script := filepath.Join(kservetesting.ProjectRoot(), "docs/samples/llmisvc/confidential-bare-metal/namespace/format-disk.sh")
	for _, tc := range []struct {
		name          string
		isLuksStatus  string
		resourceID    string
		message       string
		expectedCalls string
	}{
		{"wrong passphrase", "0", "kbs:///team/luks/key", "cannot unlock the existing LUKS volume", "isLuks\nluksOpen\n"},
		{"device error", "4", "kbs:///team/luks/key", "cannot inspect the block device", "isLuks\n"},
		{"invalid key URI", "0", "https://example.com/key", "invalid LUKS_RESOURCE_ID", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			logFile := filepath.Join(dir, "commands")
			require.NoError(t, os.WriteFile(logFile, nil, 0o600))
			//nolint:gosec // The mock executable is confined to the test's private temporary directory.
			require.NoError(t, os.WriteFile(filepath.Join(dir, "cryptsetup"), []byte(`#!/bin/sh
[ "$1" != "--key-file=-" ] || shift
printf '%s\n' "$1" >> "$CC_STORAGE_TEST_LOG"
case "$1" in
    isLuks) exit "$CC_STORAGE_TEST_STATUS" ;;
    luksOpen) cat >/dev/null; exit 1 ;;
    *) exit 99 ;;
esac
`), 0o700))
			cmd := exec.CommandContext(t.Context(), "bash", script) //nolint:gosec // Executes the repository helper with a temporary mock cryptsetup.
			cmd.Env = append(os.Environ(),
				"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"PASS=test-passphrase", "LUKS_RESOURCE_ID="+tc.resourceID,
				"CC_STORAGE_TEST_LOG="+logFile, "CC_STORAGE_TEST_STATUS="+tc.isLuksStatus,
			)
			output, err := cmd.CombinedOutput()
			require.Error(t, err)
			require.Contains(t, string(output), tc.message)
			calls, err := os.ReadFile(logFile) //nolint:gosec // The log is created in this test's private temporary directory.
			require.NoError(t, err)
			require.Equal(t, tc.expectedCalls, string(calls))
		})
	}
}

func TestConfidentialBareMetalRequiresModelKey(t *testing.T) {
	preset := loadBareMetalPreset(t, "config/overlays/odh/llmisvcconfig/config-llm-confidential-bare-metal.yaml")
	for name, confidential := range map[string]*v1alpha2.ConfidentialSpec{
		"missing confidential spec": nil,
		"missing model key":         {Enabled: true},
		"confidential disabled":     {Enabled: false, ResourceId: ptr.To("kbs:///team/model/key")},
	} {
		t.Run(name, func(t *testing.T) {
			service := llmisvc.LLMInferenceServiceSample()
			service.Spec.Model.Confidential = confidential
			merged, err := llmisvc.MergeSpecs(t.Context(), preset.Spec, service.Spec)
			require.NoError(t, err)
			service.Spec = merged
			rendered, err := llmisvc.ReplaceVariables(service, &v1alpha2.LLMInferenceServiceConfig{Spec: merged}, nil)
			require.NoError(t, err)
			initializer := utils.GetInitContainerWithName(rendered.Spec.Template, "storage-initializer")
			require.NotNil(t, initializer)
			args := append([]string{}, initializer.Command[1:]...)
			args = append(args, initializer.Args...)
			cmd := exec.CommandContext(t.Context(), initializer.Command[0], args...) //nolint:gosec // Executes the fixed repository preset wrapper with test model settings.
			cmd.Env = os.Environ()
			for _, env := range initializer.Env {
				cmd.Env = append(cmd.Env, env.Name+"="+env.Value)
			}
			output, err := cmd.CombinedOutput()
			require.Error(t, err)
			require.Contains(t, string(output), "spec.model.confidential.enabled and resourceId are required")
		})
	}
}

func TestConfidentialBareMetalCDHFallback(t *testing.T) {
	script := filepath.Join(kservetesting.ProjectRoot(), "docs/samples/llmisvc/confidential-bare-metal/namespace/format-disk.sh")
	for _, connectionDelimited := range []bool{false, true} {
		t.Run(map[bool]string{false: "content length", true: "connection close"}[connectionDelimited], func(t *testing.T) {
			requests := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case requests <- r.URL.Path:
				default:
				}
				if connectionDelimited {
					w.(http.Flusher).Flush()
				}
				_, _ = w.Write([]byte("test passphrase"))
			}))
			t.Cleanup(server.Close)
			serverURL, err := url.Parse(server.URL)
			require.NoError(t, err)
			dir := t.TempDir()
			keyFile := filepath.Join(dir, "received-passphrase")
			//nolint:gosec // Only this test's owner can access or execute the mock.
			require.NoError(t, os.WriteFile(filepath.Join(dir, "cryptsetup"), []byte(`#!/bin/sh
[ "$1" != "--key-file=-" ] || shift
case "$1" in
    isLuks) exit 0 ;;
    luksOpen) cat > "$CC_STORAGE_TEST_KEY"; exit 1 ;;
    *) exit 99 ;;
esac
`), 0o700))
			cmd := exec.CommandContext(t.Context(), "bash", script) //nolint:gosec // Executes the repository helper against the local test CDH and mock cryptsetup.
			cmd.Env = append(os.Environ(),
				"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"PASS=sealed.pointer", "LUKS_RESOURCE_ID=kbs:///team/luks-passphrase/password",
				"CDH_PORT="+serverURL.Port(), "CC_STORAGE_TEST_KEY="+keyFile,
			)
			output, err := cmd.CombinedOutput()
			require.Error(t, err, "the mock deliberately rejects the fetched passphrase")
			require.Contains(t, string(output), "cannot unlock the existing LUKS volume")
			require.Equal(t, "/cdh/resource/team/luks-passphrase/password", <-requests)
			key, err := os.ReadFile(keyFile) //nolint:gosec // Reads the fake passphrase captured in this test's temporary directory.
			require.NoError(t, err)
			require.Equal(t, "test passphrase", string(key))
		})
	}
}
