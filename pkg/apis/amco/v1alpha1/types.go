// Package v1alpha1 contains the AIXConfigPool API of the AIX Machine Config
// Operator (AMCO).
//
// An AIXConfigPool groups AIX nodes that share one rendered configuration and
// one rollout policy, in the same way a MachineConfigPool does for RHCOS
// nodes. AMCO deliberately does not reuse MachineConfigPool: the stock
// machine-config-operator would otherwise try to render MachineConfigs for the
// pool and drive its nodes.
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// AIXConfigPool selects AIX nodes, holds the name of the configuration they
// should run and reports how far the rollout of that configuration has got.
type AIXConfigPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AIXConfigPoolSpec   `json:"spec,omitempty"`
	Status AIXConfigPoolStatus `json:"status,omitempty"`
}

// AIXConfigPoolSpec is the desired state of an AIXConfigPool.
type AIXConfigPoolSpec struct {
	// NodeSelector selects the nodes in the pool. Only nodes that also carry
	// kubernetes.io/os=aix are ever managed. When unset the pool selects
	// node-role.kubernetes.io/aix.
	// +optional
	NodeSelector *metav1.LabelSelector `json:"nodeSelector,omitempty"`

	// MaxUnavailable is the number (or percentage) of nodes in the pool that
	// may be unavailable at once while a new configuration rolls out. A node
	// that is not Ready, is cordoned, or is applying configuration counts as
	// unavailable. Defaults to 1; a value that rounds down to 0 is treated as 1.
	// +optional
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`

	// Paused stops new nodes from starting an update. Updates already in
	// progress finish.
	// +optional
	Paused bool `json:"paused,omitempty"`
}

// AIXConfigPoolStatus is the observed state of an AIXConfigPool.
type AIXConfigPoolStatus struct {
	// ObservedGeneration is the generation of the spec this status describes.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// RenderedConfig is the name of the ConfigMap, in the operator
	// namespace, that holds the configuration the pool's nodes should run.
	RenderedConfig string `json:"renderedConfig,omitempty"`

	// MachineCount is the number of nodes in the pool.
	MachineCount int32 `json:"machineCount"`
	// UpdatedMachineCount is the number of nodes running RenderedConfig.
	UpdatedMachineCount int32 `json:"updatedMachineCount"`
	// ReadyMachineCount is the number of updated nodes that are Ready and schedulable.
	ReadyMachineCount int32 `json:"readyMachineCount"`
	// UnavailableMachineCount is the number of nodes counted against maxUnavailable.
	UnavailableMachineCount int32 `json:"unavailableMachineCount"`
	// DegradedMachineCount is the number of nodes whose agent reports Degraded.
	DegradedMachineCount int32 `json:"degradedMachineCount"`

	// Warnings lists inputs the renderer had to ignore because AIX nodes
	// cannot honour them (for example CRI-O-only ContainerRuntimeConfig fields).
	// +optional
	Warnings []string `json:"warnings,omitempty"`

	// Conditions use the same types as MachineConfigPool: Updated, Updating,
	// Degraded, RenderDegraded and NodeDegraded.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Condition types reported on an AIXConfigPool.
const (
	ConditionUpdated        = "Updated"
	ConditionUpdating       = "Updating"
	ConditionDegraded       = "Degraded"
	ConditionRenderDegraded = "RenderDegraded"
	ConditionNodeDegraded   = "NodeDegraded"
)

// AIXConfigPoolList is a list of AIXConfigPools.
type AIXConfigPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []AIXConfigPool `json:"items"`
}
