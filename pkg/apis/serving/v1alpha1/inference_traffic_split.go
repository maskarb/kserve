/*
Copyright 2024 The KServe Authors.

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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	duckv1 "knative.dev/pkg/apis/duck/v1"
)

// InferenceTrafficSplitSpec defines a weighted traffic split across multiple InferenceServices.
type InferenceTrafficSplitSpec struct {
	// Backends is the list of InferenceService references with traffic weights.
	// +required
	// +listType=map
	// +listMapKey=inferenceServiceRef
	Backends []TrafficSplitBackend `json:"backends"`
}

// TrafficSplitBackend references an InferenceService and assigns a traffic weight.
type TrafficSplitBackend struct {
	// InferenceServiceRef is the name of the InferenceService in the same namespace.
	// +required
	InferenceServiceRef string `json:"inferenceServiceRef"`
	// Weight is the traffic weight for this backend. Gateway API normalizes weights
	// across all backends, so values do not need to sum to 100.
	// +required
	Weight int32 `json:"weight"`
}

// InferenceTrafficSplitStatus defines the observed state of InferenceTrafficSplit.
type InferenceTrafficSplitStatus struct {
	// URL is the shared endpoint for this traffic split.
	// +optional
	URL string `json:"url,omitempty"`
	// Address is the cluster-local address.
	// +optional
	Address *duckv1.Addressable `json:"address,omitempty"`
	// Conditions represent the latest available observations of the InferenceTrafficSplit's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// InferenceTrafficSplit defines a weighted traffic split across multiple InferenceServices
// sharing a single ingress endpoint via Gateway API HTTPRoute.
// +k8s:openapi-gen=true
// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="URL",type="string",JSONPath=".status.url"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:resource:path=inferencetrafficsplits,shortName=its
type InferenceTrafficSplit struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   InferenceTrafficSplitSpec   `json:"spec,omitempty"`
	Status InferenceTrafficSplitStatus `json:"status,omitempty"`
}

// InferenceTrafficSplitList contains a list of InferenceTrafficSplit
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
type InferenceTrafficSplitList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []InferenceTrafficSplit `json:"items"`
}

func init() {
	SchemeBuilder.Register(&InferenceTrafficSplit{}, &InferenceTrafficSplitList{})
}
