package render

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/imdario/mergo"
	configv1 "github.com/openshift/api/config/v1"
	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"
	ctrlcommon "github.com/openshift/machine-config-operator/pkg/controller/common"
	kubeletconfig "github.com/openshift/machine-config-operator/pkg/controller/kubelet-config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubeletconfigv1beta1 "k8s.io/kubelet/config/v1beta1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	"github.com/gilbahat/aix-machine-config-operator/pkg/rendered"
	"github.com/gilbahat/aix-machine-config-operator/pkg/upstream"
)

// baseKubeletConfig is the AIX counterpart of the MCO's worker kubelet
// template (templates/worker/01-worker-kubelet/_base/files/kubelet.yaml). The
// differences are what AIX lacks: there are no cgroups, so pod-level QoS
// cgroups and node allocatable enforcement are off, and there is no systemd
// or static pod directory. The runtime is containerd-aix.
func baseKubeletConfig(cc *mcfgv1.ControllerConfig) *kubeletconfigv1beta1.KubeletConfiguration {
	return &kubeletconfigv1beta1.KubeletConfiguration{
		TypeMeta: metav1.TypeMeta{Kind: "KubeletConfiguration", APIVersion: kubeletconfigv1beta1.SchemeGroupVersion.String()},
		Authentication: kubeletconfigv1beta1.KubeletAuthentication{
			X509:      kubeletconfigv1beta1.KubeletX509Authentication{ClientCAFile: KubeletCAPath},
			Anonymous: kubeletconfigv1beta1.KubeletAnonymousAuthentication{Enabled: ptr.To(false)},
		},
		ClusterDNS:                []string{cc.Spec.ClusterDNSIP},
		ClusterDomain:             "cluster.local",
		ContainerLogMaxSize:       "50Mi",
		ContainerRuntimeEndpoint:  ContainerdEndpoint,
		FailSwapOn:                ptr.To(false), // AIX nodes always have paging space
		MaxPods:                   250,
		KubeAPIQPS:                ptr.To[int32](50),
		KubeAPIBurst:              100,
		RotateCertificates:        true,
		SerializeImagePulls:       ptr.To(false),
		NodeStatusUpdateFrequency: metav1.Duration{Duration: 10 * time.Second},
		NodeStatusReportFrequency: metav1.Duration{Duration: 5 * time.Minute},
		ServerTLSBootstrap:        true,
		CgroupsPerQOS:             ptr.To(false),
	}
}

// validateForAIX rejects KubeletConfiguration settings that need cgroups or
// systemd and so cannot work on AIX, in addition to the fields the MCO itself
// forbids (upstream.ValidateUserKubeletConfig).
func validateForAIX(kc *mcfgv1.KubeletConfig, cfg *kubeletconfigv1beta1.KubeletConfiguration) error {
	bad := func(field string) error {
		return fmt.Errorf("KubeletConfig %s: %s cannot be set on AIX nodes (AIX has no cgroups)", kc.Name, field)
	}
	switch {
	case cfg.CgroupsPerQOS != nil && *cfg.CgroupsPerQOS:
		return bad("cgroupsPerQOS")
	case len(cfg.EnforceNodeAllocatable) > 0:
		return bad("enforceNodeAllocatable")
	case cfg.CgroupRoot != "":
		return bad("cgroupRoot")
	case cfg.SystemCgroups != "", cfg.SystemReservedCgroup != "", cfg.KubeReservedCgroup != "", cfg.KubeletCgroups != "":
		return bad("systemCgroups/systemReservedCgroup/kubeReservedCgroup/kubeletCgroups")
	case cfg.PodPidsLimit != nil:
		return bad("podPidsLimit")
	case cfg.CPUManagerPolicy != "" && cfg.CPUManagerPolicy != "none":
		return bad("cpuManagerPolicy")
	case cfg.MemoryManagerPolicy != "" && cfg.MemoryManagerPolicy != "None":
		return bad("memoryManagerPolicy")
	case cfg.TopologyManagerPolicy != "" && cfg.TopologyManagerPolicy != "none":
		return bad("topologyManagerPolicy")
	case cfg.ContainerRuntimeEndpoint != "" && cfg.ContainerRuntimeEndpoint != ContainerdEndpoint:
		return fmt.Errorf("KubeletConfig %s: containerRuntimeEndpoint is managed by AMCO (%s)", kc.Name, ContainerdEndpoint)
	}
	return nil
}

// renderKubelet merges the KubeletConfigs targeting the pool (oldest first)
// over the AIX base, the way the MCO's generateKubeletIgnFiles merges a
// KubeletConfig over its template: user fields override, feature gates are
// not user-settable, and the TLS profile comes from the KubeletConfig or,
// failing that, the cluster APIServer config.
func renderKubelet(cc *mcfgv1.ControllerConfig, apiServer *configv1.APIServer, kcs []*mcfgv1.KubeletConfig, rt runtimeResult) (rendered.File, []string, error) {
	var warnings []string
	cfg := baseKubeletConfig(cc)
	if cc.Spec.ClusterDNSIP == "" {
		return rendered.File{}, nil, fmt.Errorf("ControllerConfig has no clusterDNSIP yet")
	}
	if rt.containerLogMaxSize != "" {
		cfg.ContainerLogMaxSize = rt.containerLogMaxSize
	}
	tlsMin, tlsCiphers := ctrlcommon.GetSecurityProfileCiphersFromAPIServer(apiServer)

	for _, kc := range kcs {
		if err := upstream.ValidateUserKubeletConfig(kc); err != nil {
			return rendered.File{}, nil, fmt.Errorf("KubeletConfig %s: %w", kc.Name, err)
		}
		if kc.Spec.TLSSecurityProfile != nil {
			tlsMin, tlsCiphers = ctrlcommon.GetSecurityProfileCiphers(kc.Spec.TLSSecurityProfile)
		}
		if kc.Spec.AutoSizingReserved != nil && *kc.Spec.AutoSizingReserved {
			warnings = append(warnings, fmt.Sprintf("KubeletConfig %s: autoSizingReserved is ignored on AIX nodes (it is a systemd helper on RHCOS)", kc.Name))
		}
		if kc.Spec.LogLevel != nil {
			warnings = append(warnings, fmt.Sprintf("KubeletConfig %s: logLevel is ignored on AIX nodes; set -v in the kubelet's SRC definition", kc.Name))
		}
		if kc.Spec.KubeletConfig == nil || kc.Spec.KubeletConfig.Raw == nil {
			continue
		}
		user, err := kubeletconfig.DecodeKubeletConfig(kc.Spec.KubeletConfig.Raw)
		if err != nil {
			return rendered.File{}, nil, fmt.Errorf("KubeletConfig %s: %w", kc.Name, err)
		}
		if err := validateForAIX(kc, user); err != nil {
			return rendered.File{}, nil, err
		}
		user.FeatureGates = nil
		if err := mergo.Merge(cfg, user, mergo.WithOverride); err != nil {
			return rendered.File{}, nil, fmt.Errorf("KubeletConfig %s: merging: %w", kc.Name, err)
		}
	}
	cfg.TLSMinVersion = tlsMin
	cfg.TLSCipherSuites = tlsCiphers

	data, err := encodeKubeletConfig(cfg)
	if err != nil {
		return rendered.File{}, nil, err
	}
	return amcoFile(KubeletConfigPath, data, restartKubelet), warnings, nil
}

// encodeKubeletConfig serializes cfg as YAML. enforceNodeAllocatable is
// written as an explicit empty list: the field is omitempty, and an omitted
// value defaults to ["pods"], which the kubelet rejects when cgroupsPerQOS is
// false.
func encodeKubeletConfig(cfg *kubeletconfigv1beta1.KubeletConfiguration) (string, error) {
	js, err := kubeletconfig.EncodeKubeletConfig(cfg, kubeletconfigv1beta1.SchemeGroupVersion, runtime.ContentTypeJSON)
	if err != nil {
		return "", fmt.Errorf("encoding kubelet configuration: %w", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(js, &m); err != nil {
		return "", err
	}
	m["enforceNodeAllocatable"] = []string{}
	out, err := yaml.Marshal(m)
	if err != nil {
		return "", err
	}
	return "# Generated by the AIX Machine Config Operator. Do not edit.\n" + strings.TrimSpace(string(out)) + "\n", nil
}
