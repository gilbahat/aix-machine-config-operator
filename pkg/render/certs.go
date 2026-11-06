package render

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"

	"github.com/gilbahat/aix-machine-config-operator/pkg/rendered"
)

// Node paths AMCO writes. They match the paths the MCO uses on RHCOS where an
// AIX equivalent exists, so tooling and docs carry over.
const (
	// KubeletCAPath is the kubelet's clientCAFile (MCO: same path).
	KubeletCAPath = "/etc/kubernetes/kubelet-ca.crt"
	// KubeletConfigPath is the kubelet's --config file (MCO: same path).
	KubeletConfigPath = "/etc/kubernetes/kubelet.conf"
	// TrustBundlePath holds the cluster's additional trust bundle. Go on AIX
	// loads every file in /var/ssl/certs as a system root, which is how
	// containerd-aix and the kubelet pick it up (MCO writes it to
	// /etc/pki/ca-trust/source/anchors on RHCOS).
	TrustBundlePath = "/var/ssl/certs/amco-additional-trust-bundle.pem"
	// CertsDir is containerd's registry host configuration directory; it is
	// containerd-aix's default config_path.
	CertsDir = "/etc/containerd/certs.d"
	// ContainerdDropInPath is AMCO's containerd configuration drop-in. The
	// node's /etc/containerd/config.toml must import /etc/containerd/conf.d/*.toml.
	ContainerdDropInPath = "/etc/containerd/conf.d/50-amco.toml"
	// ContainerdEndpoint is containerd-aix's CRI socket.
	ContainerdEndpoint = "unix:///var/run/containerd/containerd.sock"
)

// registryHostRE matches the host[:port] a registry CA bundle may be filed
// under. It keeps bundle names from escaping CertsDir.
var registryHostRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?$`)

// What a change to one of AMCO's own files needs on the node.
type fileEffect int

const (
	// live: containerd reads certs.d on every pull; the kubelet reloads
	// its client CA file.
	live fileEffect = iota
	// restartContainerd: Go reads the system roots once, and containerd
	// reads its configuration at start. Running containers survive a
	// containerd restart (shim v2), so there is no drain.
	restartContainerd
	// restartKubelet: the kubelet reads its configuration at start. This
	// keeps the MCO's behaviour for kubelet configuration: drain first.
	restartKubelet
)

func amcoFile(p, contents string, effect fileEffect) rendered.File {
	f := rendered.File{Path: p, Mode: 0o644, Contents: contents, Origin: rendered.OriginAMCO}
	switch effect {
	case restartContainerd:
		f.Actions = []rendered.SubsystemAction{{Subsystem: rendered.SubsystemContainerd, Action: rendered.ActionRestart, IgnoreMissing: true}}
	case restartKubelet:
		f.Actions = []rendered.SubsystemAction{{Subsystem: rendered.SubsystemKubelet, Action: rendered.ActionRestart, IgnoreMissing: true}}
		f.Drain = true
	}
	return f
}

func renderCertificates(cc *mcfgv1.ControllerConfig) ([]rendered.File, error) {
	var files []rendered.File
	if len(cc.Spec.KubeAPIServerServingCAData) > 0 {
		files = append(files, amcoFile(KubeletCAPath, string(cc.Spec.KubeAPIServerServingCAData), live))
	}
	if len(cc.Spec.AdditionalTrustBundle) > 0 {
		files = append(files, amcoFile(TrustBundlePath, string(cc.Spec.AdditionalTrustBundle), restartContainerd))
	}
	cas, err := registryCAs(cc)
	if err != nil {
		return nil, err
	}
	for host, data := range cas {
		files = append(files, amcoFile(registryCAPath(host), data, live))
	}
	return files, nil
}

// registryCAs returns the per-registry CA bundles the MCO collected from the
// image registry operator and from the Image config's additionalTrustedCA,
// keyed by host[:port]. The MCO files them as "host..port" (a ConfigMap key
// cannot contain ':'); user bundles are listed after the cluster's and are
// appended to them, as the MCO does when writing /etc/docker/certs.d.
func registryCAs(cc *mcfgv1.ControllerConfig) (map[string]string, error) {
	out := map[string]string{}
	for _, list := range [][]mcfgv1.ImageRegistryBundle{cc.Spec.ImageRegistryBundleData, cc.Spec.ImageRegistryBundleUserData} {
		for _, b := range list {
			host := strings.ReplaceAll(b.File, "..", ":")
			if !registryHostRE.MatchString(host) {
				return nil, fmt.Errorf("image registry CA bundle %q is not filed under a registry host name", b.File)
			}
			data := string(b.Data)
			if prev, ok := out[host]; ok && prev != data {
				if !strings.HasSuffix(prev, "\n") {
					prev += "\n"
				}
				data = prev + data
			}
			out[host] = data
		}
	}
	return out, nil
}

func registryCAPath(host string) string {
	return path.Join(CertsDir, host, "ca.crt")
}
