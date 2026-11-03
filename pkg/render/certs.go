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

func renderCertificates(cc *mcfgv1.ControllerConfig) ([]rendered.File, error) {
	var files []rendered.File
	if len(cc.Spec.KubeAPIServerServingCAData) > 0 {
		files = append(files, rendered.File{Path: KubeletCAPath, Mode: 0o644, Contents: string(cc.Spec.KubeAPIServerServingCAData), Group: rendered.GroupKubeletCA})
	}
	if len(cc.Spec.AdditionalTrustBundle) > 0 {
		files = append(files, rendered.File{Path: TrustBundlePath, Mode: 0o644, Contents: string(cc.Spec.AdditionalTrustBundle), Group: rendered.GroupTrust})
	}
	cas, err := registryCAs(cc)
	if err != nil {
		return nil, err
	}
	for host, data := range cas {
		files = append(files, rendered.File{Path: registryCAPath(host), Mode: 0o644, Contents: data, Group: rendered.GroupRegistries})
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
