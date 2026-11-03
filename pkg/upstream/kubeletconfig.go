// Code in this file is copied from github.com/openshift/machine-config-operator
// pkg/controller/kubelet-config/helpers.go at commit
// 0e2557038232d89cfbf810a6aabf85430d472211 (branch release-5.0),
// licensed under the Apache License, Version 2.0. The function is unexported
// upstream, so it is copied here verbatim apart from being exported and
// calling the exported upstream DecodeKubeletConfig through its package.
// Keep it in sync when the MCO pin moves (see hack/check-upstream-copies.sh).

package upstream

import (
	"fmt"

	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"
	kubeletconfig "github.com/openshift/machine-config-operator/pkg/controller/kubelet-config"
)

// ValidateUserKubeletConfig is upstream validateUserKubeletConfig.
func ValidateUserKubeletConfig(cfg *mcfgv1.KubeletConfig) error {
	if cfg.Spec.LogLevel != nil && (*cfg.Spec.LogLevel < 0 || *cfg.Spec.LogLevel > 10) {
		return fmt.Errorf("KubeletConfig's LogLevel is not valid [0,10]: %d", *cfg.Spec.LogLevel)
	}
	if cfg.Spec.KubeletConfig == nil || cfg.Spec.KubeletConfig.Raw == nil {
		return nil
	}
	kcDecoded, err := kubeletconfig.DecodeKubeletConfig(cfg.Spec.KubeletConfig.Raw)
	if err != nil {
		return fmt.Errorf("KubeletConfig could not be unmarshalled, err: %w", err)
	}

	// Check all the fields a user cannot set within the KubeletConfig CR.
	// If a user were to set these values, the system may become unrecoverable
	// (ie: not recover after a reboot).
	// Therefore, if the KubeletConfig CR instance contains a non-zero or non-empty value
	// for one of the following fields, the MCC will not apply the CR and error out instead.
	if kcDecoded.CgroupDriver != "" {
		return fmt.Errorf("KubeletConfiguration: cgroupDriver is not allowed to be set, but contains: %s", kcDecoded.CgroupDriver)
	}
	if len(kcDecoded.ClusterDNS) > 0 {
		return fmt.Errorf("KubeletConfiguration: clusterDNS is not allowed to be set, but contains: %s", kcDecoded.ClusterDNS)
	}
	if kcDecoded.ClusterDomain != "" {
		return fmt.Errorf("KubeletConfiguration: clusterDomain is not allowed to be set, but contains: %s", kcDecoded.ClusterDomain)
	}
	if len(kcDecoded.FeatureGates) > 0 {
		return fmt.Errorf("KubeletConfiguration: featureGates is not allowed to be set, but contains: %v", kcDecoded.FeatureGates)
	}
	if kcDecoded.FailSwapOn != nil {
		return fmt.Errorf("KubeletConfiguration: failSwapOn is not allowed to be set, but contains: %v", *kcDecoded.FailSwapOn)
	}
	if kcDecoded.MemorySwap.SwapBehavior != "" {
		return fmt.Errorf("KubeletConfiguration: swapBehavior is not allowed to be set, but contains: %s", kcDecoded.MemorySwap.SwapBehavior)
	}
	if kcDecoded.SystemReserved != nil && len(kcDecoded.SystemReserved) > 0 &&
		cfg.Spec.AutoSizingReserved != nil && *cfg.Spec.AutoSizingReserved {
		return fmt.Errorf("KubeletConfiguration: autoSizingReserved and systemdReserved cannot be set together")
	}
	// Validate that systemReservedCgroup matches systemCgroups if both are set
	if kcDecoded.SystemReservedCgroup != "" && kcDecoded.SystemCgroups != "" && kcDecoded.SystemReservedCgroup != kcDecoded.SystemCgroups {
		return fmt.Errorf("KubeletConfiguration: systemReservedCgroup (%s) must match systemCgroups (%s)", kcDecoded.SystemReservedCgroup, kcDecoded.SystemCgroups)
	}
	return nil
}
