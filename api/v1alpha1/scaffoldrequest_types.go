/*
Copyright 2026.

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
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// ComponentReference identifies the platform.taskapp.io/v1alpha1 Component
// this resource is correlated with, matching the {name} shape every related
// CR uses per PLATFORM_API_ARCHITECTURE.md rule 5.
type ComponentReference struct {
	// name of the Component this resource belongs to.
	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

// ScaffoldRequestSpec defines the desired state of ScaffoldRequest. It is
// entirely self-contained: component-operator resolves every field once, at
// creation time, from the Component and its now-ready GitHubRepository. The
// scaffold-operator controller that executes this request MUST NEVER Get the
// referenced Component or otherwise resolve execution inputs from it —
// everything needed to execute already lives in the fields below. This is
// the hard boundary between "decides WHAT/WHEN" (component-operator) and
// "executes" (scaffold-operator); do not blur it later for convenience.
type ScaffoldRequestSpec struct {
	// componentRef is informational: correlation/provenance only (matches the
	// standard componentRef + platform.taskapp.io/component label pattern
	// every related resource carries). Not read by this controller as an
	// execution input.
	// +kubebuilder:validation:Required
	ComponentRef ComponentReference `json:"componentRef"`

	// componentName is the identity used for the service/binary name, Helm
	// chart name, and Kubernetes resource names rendered into the scaffold.
	// +kubebuilder:validation:Required
	ComponentName string `json:"componentName"`

	// repositoryName is the name of the target GitHub repository the
	// scaffold is committed into, and is also substituted into the rendered
	// template (e.g. the Go module path).
	// +kubebuilder:validation:Required
	RepositoryName string `json:"repositoryName"`

	// owner is the GitHub org or user the target repository belongs to.
	// +kubebuilder:validation:Required
	Owner string `json:"owner"`

	// template is the platform-scaffolds template name (e.g. golang-service).
	// +kubebuilder:validation:Required
	Template string `json:"template"`

	// version is the platform-scaffolds template's SemVer tag (e.g. 0.1.0),
	// without the template name prefix or a leading "v".
	// +kubebuilder:validation:Required
	Version string `json:"version"`
}

// ScaffoldRequestStatus defines the observed state of ScaffoldRequest.
type ScaffoldRequestStatus struct {
	// conditions represent the current state of the ScaffoldRequest resource.
	//
	// Two terminal condition types are used, and neither is ever cleared or
	// retried automatically once True:
	//   - "Completed": True on success, including the crash-recovery case
	//     where a prior run's commit is verified to already exist.
	//   - "Blocked": True when the target repository has unexplained
	//     pre-existing content this request did not create and cannot prove
	//     it created — a terminal, human-intervention-required state (e.g.
	//     reason "RepositoryNotEmpty").
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// templateRevision is the immutable platform-scaffolds commit SHA
	// actually fetched and rendered — distinct from spec.version (the
	// human-readable SemVer tag). Set as soon as the tag is resolved, before
	// any GitHub write is attempted, so it's available even if the run later
	// fails or blocks.
	// +optional
	TemplateRevision string `json:"templateRevision,omitempty"`

	// commitSHA is the commit created in the target application repository —
	// the other half of provenance, answering "what did we write" as opposed
	// to templateRevision's "what did we render it from". Also the recovery
	// anchor for the once-only check performed on every reconcile of a
	// non-empty repository.
	// +optional
	CommitSHA string `json:"commitSHA,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// ScaffoldRequest is the Schema for the scaffoldrequests API
type ScaffoldRequest struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ScaffoldRequest
	// +required
	Spec ScaffoldRequestSpec `json:"spec"`

	// status defines the observed state of ScaffoldRequest
	// +optional
	Status ScaffoldRequestStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ScaffoldRequestList contains a list of ScaffoldRequest
type ScaffoldRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ScaffoldRequest `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ScaffoldRequest{}, &ScaffoldRequestList{})
}
