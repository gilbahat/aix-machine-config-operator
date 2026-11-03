// Package nodestate defines the node labels and annotations through which the
// AMCO operator and the AIX node agent coordinate. It has no dependencies
// beyond the Kubernetes core types so the agent stays small.
//
// The protocol mirrors the machine-config-daemon's, under AMCO's own prefix so
// that the stock MCO never reads or writes these keys:
//
//	operator sets  desiredConfig        -> agent applies it
//	agent sets     state=Working        while applying
//	agent sets     currentConfig, state=Done (or state=Degraded + reason)
package nodestate

import (
	corev1 "k8s.io/api/core/v1"
)

const prefix = "amco.reuna-labs.io/"

// Labels.
const (
	// RoleLabel marks a node as an AIX node managed by AMCO. It replaces the
	// worker role: the stock MCO's worker pool selects
	// node-role.kubernetes.io/worker, so AIX nodes without that label are
	// never part of an MCO MachineConfigPool.
	RoleLabel = "node-role.kubernetes.io/aix"
	// WorkerRoleLabel is the role label AIX nodes must not carry.
	WorkerRoleLabel = "node-role.kubernetes.io/worker"
	// OSLabel is set by the kubelet from runtime.GOOS.
	OSLabel = corev1.LabelOSStable
	// OSAIX is the OSLabel value of an AIX node.
	OSAIX = "aix"
)

// Annotations.
const (
	// DesiredConfigAnnotation is written by the operator: the name of the
	// rendered config the node should run.
	DesiredConfigAnnotation = prefix + "desiredConfig"
	// CurrentConfigAnnotation is written by the agent once a config is applied.
	CurrentConfigAnnotation = prefix + "currentConfig"
	// StateAnnotation is written by the agent; one of the State* values.
	StateAnnotation = prefix + "state"
	// ReasonAnnotation is written by the agent with the error behind a Degraded state.
	ReasonAnnotation = prefix + "reason"
	// AgentVersionAnnotation is written by the agent at start-up.
	AgentVersionAnnotation = prefix + "agentVersion"
	// OSLevelAnnotation is written by the agent at start-up (oslevel -s).
	OSLevelAnnotation = prefix + "osLevel"
	// CordonedByAnnotation is written by the operator when it cordons a node
	// for an update, so it only uncordons nodes it cordoned itself. The value
	// is the rendered config the node was cordoned for.
	CordonedByAnnotation = prefix + "cordonedFor"
)

// Agent states.
const (
	StateDone     = "Done"
	StateWorking  = "Working"
	StateDegraded = "Degraded"
)

// IsAIX reports whether the kubelet identifies node as an AIX node.
func IsAIX(node *corev1.Node) bool {
	return node.Labels[OSLabel] == OSAIX
}

// Desired returns the node's desired config annotation.
func Desired(node *corev1.Node) string { return node.Annotations[DesiredConfigAnnotation] }

// Current returns the node's current config annotation.
func Current(node *corev1.Node) string { return node.Annotations[CurrentConfigAnnotation] }

// State returns the node's agent state annotation.
func State(node *corev1.Node) string { return node.Annotations[StateAnnotation] }

// IsDone reports whether the agent on node has finished applying target.
func IsDone(node *corev1.Node, target string) bool {
	return target != "" && Desired(node) == target && Current(node) == target && State(node) == StateDone
}

// IsReady reports whether the node's Ready condition is True.
func IsReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
