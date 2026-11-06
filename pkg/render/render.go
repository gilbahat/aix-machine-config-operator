// Package render turns the cluster's node configuration objects into the
// rendered.Config for a pool of AIX nodes.
//
// The inputs are the same objects the stock MCO renders RHCOS configuration
// from: the ControllerConfig (which the MCO keeps filled with the cluster's CA
// bundles and cluster DNS address), the cluster Image config, ImageDigestMirrorSets,
// ImageTagMirrorSets, ContainerRuntimeConfigs and KubeletConfigs. Where the MCO
// writes CRI-O and systemd files, AMCO writes their containerd-aix and AIX
// kubelet equivalents.
//
// On top of that plumbing come the AIXMachineConfigs the pool selects: host
// configuration (files, SRC subsystems, inittab entries, tunables, device
// attributes) merged in name order.
package render

import (
	"fmt"
	"sort"

	configv1 "github.com/openshift/api/config/v1"
	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	amcov1alpha1 "github.com/gilbahat/aix-machine-config-operator/pkg/apis/amco/v1alpha1"
	"github.com/gilbahat/aix-machine-config-operator/pkg/rendered"
)

// Inputs are the cluster objects a pool's configuration is rendered from.
// Slices may contain objects that do not target the pool; Render filters
// KubeletConfigs and ContainerRuntimeConfigs by their machineConfigPoolSelector
// against the AIXConfigPool's labels, exactly as the MCO matches them against
// MachineConfigPool labels.
type Inputs struct {
	Pool             *amcov1alpha1.AIXConfigPool
	ControllerConfig *mcfgv1.ControllerConfig
	// Image is the cluster Image config; may be nil.
	Image *configv1.Image
	// APIServer is the cluster APIServer config; its TLS profile is the
	// kubelet's default. May be nil.
	APIServer               *configv1.APIServer
	ImageDigestMirrorSets   []*configv1.ImageDigestMirrorSet
	ImageTagMirrorSets      []*configv1.ImageTagMirrorSet
	ContainerRuntimeConfigs []*mcfgv1.ContainerRuntimeConfig
	KubeletConfigs          []*mcfgv1.KubeletConfig
	// MachineConfigs are filtered by the pool's configSelector and merged
	// in name order.
	MachineConfigs []*amcov1alpha1.AIXMachineConfig
}

// Result is a rendered config plus anything the renderer had to ignore.
type Result struct {
	Config *rendered.Config
	// Warnings describe inputs that cannot take effect on AIX nodes.
	Warnings []string
}

// Render produces the configuration for in.Pool.
func Render(in Inputs) (*Result, error) {
	if in.Pool == nil {
		return nil, fmt.Errorf("no pool")
	}
	if in.ControllerConfig == nil {
		return nil, fmt.Errorf("ControllerConfig is not available yet")
	}
	res := &Result{Config: &rendered.Config{Version: rendered.FormatVersion, Pool: in.Pool.Name}}

	kcs, err := matchingKubeletConfigs(in.Pool, in.KubeletConfigs)
	if err != nil {
		return nil, err
	}
	crcs, err := matchingContainerRuntimeConfigs(in.Pool, in.ContainerRuntimeConfigs)
	if err != nil {
		return nil, err
	}

	certs, err := renderCertificates(in.ControllerConfig)
	if err != nil {
		return nil, err
	}
	res.add(certs...)

	regFiles, warnings, err := renderRegistries(in.ControllerConfig, in.Image, in.ImageDigestMirrorSets, in.ImageTagMirrorSets)
	if err != nil {
		return nil, fmt.Errorf("rendering registries: %w", err)
	}
	res.add(regFiles...)
	res.Warnings = append(res.Warnings, warnings...)

	runtime, warnings, err := renderContainerRuntime(crcs)
	if err != nil {
		return nil, fmt.Errorf("rendering ContainerRuntimeConfig: %w", err)
	}
	res.add(runtime.files...)
	res.Warnings = append(res.Warnings, warnings...)

	kubelet, warnings, err := renderKubelet(in.ControllerConfig, in.APIServer, kcs, runtime)
	if err != nil {
		return nil, fmt.Errorf("rendering KubeletConfig: %w", err)
	}
	res.add(kubelet)
	res.Warnings = append(res.Warnings, warnings...)

	mcs, err := selectMachineConfigs(in.Pool, in.MachineConfigs)
	if err != nil {
		return nil, err
	}
	warnings, err = mergeMachineConfigs(res.Config, mcs)
	if err != nil {
		return nil, err
	}
	res.Warnings = append(res.Warnings, warnings...)

	if err := res.Config.Validate(); err != nil {
		return nil, err
	}
	res.Config.Normalize()
	sort.Strings(res.Warnings)
	return res, nil
}

func (r *Result) add(files ...rendered.File) {
	r.Config.Files = append(r.Config.Files, files...)
}

// poolSelected reports whether a machineConfigPoolSelector selects pool. A
// nil selector selects nothing, as in the MCO.
func poolSelected(sel *metav1.LabelSelector, pool *amcov1alpha1.AIXConfigPool) (bool, error) {
	if sel == nil {
		return false, nil
	}
	s, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return false, err
	}
	return s.Matches(labels.Set(pool.Labels)), nil
}

// matchingKubeletConfigs returns the KubeletConfigs that target pool, oldest
// first, so that later ones override earlier ones when merged.
func matchingKubeletConfigs(pool *amcov1alpha1.AIXConfigPool, all []*mcfgv1.KubeletConfig) ([]*mcfgv1.KubeletConfig, error) {
	var out []*mcfgv1.KubeletConfig
	for _, kc := range all {
		ok, err := poolSelected(kc.Spec.MachineConfigPoolSelector, pool)
		if err != nil {
			return nil, fmt.Errorf("KubeletConfig %s: invalid machineConfigPoolSelector: %w", kc.Name, err)
		}
		if ok {
			out = append(out, kc)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return olderFirst(&out[i].ObjectMeta, &out[j].ObjectMeta) })
	return out, nil
}

func matchingContainerRuntimeConfigs(pool *amcov1alpha1.AIXConfigPool, all []*mcfgv1.ContainerRuntimeConfig) ([]*mcfgv1.ContainerRuntimeConfig, error) {
	var out []*mcfgv1.ContainerRuntimeConfig
	for _, c := range all {
		ok, err := poolSelected(c.Spec.MachineConfigPoolSelector, pool)
		if err != nil {
			return nil, fmt.Errorf("ContainerRuntimeConfig %s: invalid machineConfigPoolSelector: %w", c.Name, err)
		}
		if ok {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return olderFirst(&out[i].ObjectMeta, &out[j].ObjectMeta) })
	return out, nil
}

func olderFirst(a, b *metav1.ObjectMeta) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.Name < b.Name
}
