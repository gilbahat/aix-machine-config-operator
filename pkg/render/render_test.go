package render

import (
	"strings"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	amcov1alpha1 "github.com/gilbahat/aix-machine-config-operator/pkg/apis/amco/v1alpha1"
	"github.com/gilbahat/aix-machine-config-operator/pkg/rendered"
)

const poolLabel = amcov1alpha1.PoolLabelPrefix + "aix"

func testPool() *amcov1alpha1.AIXConfigPool {
	return &amcov1alpha1.AIXConfigPool{ObjectMeta: metav1.ObjectMeta{Name: "aix", Labels: map[string]string{poolLabel: ""}}}
}

func poolSelector() *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{poolLabel: ""}}
}

func testCC() *mcfgv1.ControllerConfig {
	return &mcfgv1.ControllerConfig{Spec: mcfgv1.ControllerConfigSpec{
		ClusterDNSIP:               "172.30.0.10",
		KubeAPIServerServingCAData: []byte("KUBELET-CA\n"),
		AdditionalTrustBundle:      []byte("TRUST\n"),
		ImageRegistryBundleData:    []mcfgv1.ImageRegistryBundle{{File: "image-registry.openshift-image-registry.svc..5000", Data: []byte("REGCA\n")}},
		ImageRegistryBundleUserData: []mcfgv1.ImageRegistryBundle{
			{File: "mirror.example.com..5000", Data: []byte("MIRRORCA\n")},
		},
	}}
}

func files(t *testing.T, res *Result) map[string]rendered.File {
	t.Helper()
	m := map[string]rendered.File{}
	for _, f := range res.Config.Files {
		m[f.Path] = f
	}
	return m
}

func TestRenderCertificates(t *testing.T) {
	res, err := Render(Inputs{Pool: testPool(), ControllerConfig: testCC()})
	require.NoError(t, err)
	fs := files(t, res)
	assert.Equal(t, "KUBELET-CA\n", fs[KubeletCAPath].Contents)
	assert.Equal(t, rendered.GroupKubeletCA, fs[KubeletCAPath].Group)
	assert.Equal(t, "TRUST\n", fs[TrustBundlePath].Contents)
	assert.Equal(t, rendered.GroupTrust, fs[TrustBundlePath].Group)
	assert.Equal(t, "REGCA\n", fs["/etc/containerd/certs.d/image-registry.openshift-image-registry.svc:5000/ca.crt"].Contents)
	hosts := fs["/etc/containerd/certs.d/image-registry.openshift-image-registry.svc:5000/hosts.toml"].Contents
	assert.Contains(t, hosts, `server = "https://image-registry.openshift-image-registry.svc:5000"`)
	assert.Contains(t, hosts, `ca = "/etc/containerd/certs.d/image-registry.openshift-image-registry.svc:5000/ca.crt"`)
}

func TestRegistryCABundleNameCannotEscape(t *testing.T) {
	cc := testCC()
	cc.Spec.ImageRegistryBundleUserData = []mcfgv1.ImageRegistryBundle{{File: "../../etc..22", Data: []byte("x")}}
	_, err := Render(Inputs{Pool: testPool(), ControllerConfig: cc})
	assert.ErrorContains(t, err, "not filed under a registry host name")
}

func TestRenderMirrors(t *testing.T) {
	idms := &configv1.ImageDigestMirrorSet{ObjectMeta: metav1.ObjectMeta{Name: "release"}, Spec: configv1.ImageDigestMirrorSetSpec{ImageDigestMirrors: []configv1.ImageDigestMirrors{
		// prefix-only remap: expressible
		{Source: "quay.io/openshift-release-dev/ocp-release", Mirrors: []configv1.ImageMirror{"mirror.example.com:5000/quay/openshift-release-dev/ocp-release"}},
		// renaming remap: not expressible
		{Source: "quay.io/openshift-release-dev/ocp-v4.0-art-dev", Mirrors: []configv1.ImageMirror{"mirror.example.com:5000/ocp4/art"}},
		// whole registry, never contact source
		{Source: "registry.redhat.io", Mirrors: []configv1.ImageMirror{"mirror.example.com:5000/redhat"}, MirrorSourcePolicy: configv1.NeverContactSource},
	}}}
	itms := &configv1.ImageTagMirrorSet{ObjectMeta: metav1.ObjectMeta{Name: "tags"}, Spec: configv1.ImageTagMirrorSetSpec{ImageTagMirrors: []configv1.ImageTagMirrors{
		{Source: "docker.io", Mirrors: []configv1.ImageMirror{"hub-mirror.example.com"}},
	}}}
	img := &configv1.Image{Spec: configv1.ImageSpec{RegistrySources: configv1.RegistrySources{
		InsecureRegistries: []string{"insecure.example.com"},
		BlockedRegistries:  []string{"blocked.example.com"},
	}}}
	res, err := Render(Inputs{Pool: testPool(), ControllerConfig: testCC(), Image: img,
		ImageDigestMirrorSets: []*configv1.ImageDigestMirrorSet{idms}, ImageTagMirrorSets: []*configv1.ImageTagMirrorSet{itms}})
	require.NoError(t, err)
	fs := files(t, res)

	quay := fs["/etc/containerd/certs.d/quay.io/hosts.toml"].Contents
	assert.Contains(t, quay, `server = "https://quay.io"`)
	assert.Contains(t, quay, `[host."https://mirror.example.com:5000/v2/quay"]`)
	assert.Contains(t, quay, `capabilities = ["pull"]`)
	assert.Contains(t, quay, `override_path = true`)
	assert.Contains(t, quay, `ca = "/etc/containerd/certs.d/mirror.example.com:5000/ca.crt"`)
	assert.NotContains(t, quay, "ocp4/art")

	rh := fs["/etc/containerd/certs.d/registry.redhat.io/hosts.toml"].Contents
	assert.Contains(t, rh, `server = "https://mirror.example.com:5000/v2/redhat"`)
	assert.NotContains(t, rh, "https://registry.redhat.io")
	assert.NotContains(t, rh, "[host.")

	hub := fs["/etc/containerd/certs.d/docker.io/hosts.toml"].Contents
	assert.Contains(t, hub, `server = "https://registry-1.docker.io"`)
	assert.Contains(t, hub, `[host."https://hub-mirror.example.com"]`)
	assert.Contains(t, hub, `capabilities = ["pull", "resolve"]`)

	insecure := fs["/etc/containerd/certs.d/insecure.example.com/hosts.toml"].Contents
	assert.Contains(t, insecure, `server = "http://insecure.example.com"`)
	assert.Contains(t, insecure, "skip_verify = true")

	joined := strings.Join(res.Warnings, "\n")
	assert.Contains(t, joined, "ocp-v4.0-art-dev")
	assert.Contains(t, joined, `blocked registry "blocked.example.com" is not enforced`)
	assert.Contains(t, joined, `mirrors of "quay.io/openshift-release-dev/ocp-release" apply to every repository on quay.io`)
}

func TestRenderRejectsBadHosts(t *testing.T) {
	for _, scope := range []string{"..", "../etc", "-x.example.com"} {
		img := &configv1.Image{Spec: configv1.ImageSpec{RegistrySources: configv1.RegistrySources{InsecureRegistries: []string{scope}}}}
		_, err := Render(Inputs{Pool: testPool(), ControllerConfig: testCC(), Image: img})
		assert.Error(t, err, scope)
	}
}

func TestRenderRejectsInvalidScopes(t *testing.T) {
	img := &configv1.Image{Spec: configv1.ImageSpec{RegistrySources: configv1.RegistrySources{InsecureRegistries: []string{"*.example.com/ns"}}}}
	_, err := Render(Inputs{Pool: testPool(), ControllerConfig: testCC(), Image: img})
	assert.ErrorContains(t, err, "invalid entry for insecure registries")
}

func kubeletConfig(name string, sel *metav1.LabelSelector, raw string) *mcfgv1.KubeletConfig {
	kc := &mcfgv1.KubeletConfig{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: mcfgv1.KubeletConfigSpec{MachineConfigPoolSelector: sel}}
	if raw != "" {
		kc.Spec.KubeletConfig = &runtime.RawExtension{Raw: []byte(raw)}
	}
	return kc
}

func TestRenderKubelet(t *testing.T) {
	older := kubeletConfig("a", poolSelector(), `{"maxPods": 100, "kubeAPIBurst": 200}`)
	older.CreationTimestamp = metav1.Unix(1, 0)
	newer := kubeletConfig("b", poolSelector(), `{"maxPods": 120}`)
	newer.CreationTimestamp = metav1.Unix(2, 0)
	newer.Spec.TLSSecurityProfile = &configv1.TLSSecurityProfile{Type: configv1.TLSProfileModernType}
	other := kubeletConfig("worker-only", &metav1.LabelSelector{MatchLabels: map[string]string{"pools.operator.machineconfiguration.openshift.io/worker": ""}}, `{"maxPods": 7}`)
	unselected := kubeletConfig("nil-selector", nil, `{"maxPods": 8}`)

	res, err := Render(Inputs{Pool: testPool(), ControllerConfig: testCC(), KubeletConfigs: []*mcfgv1.KubeletConfig{newer, other, older, unselected}})
	require.NoError(t, err)
	kubelet := files(t, res)[KubeletConfigPath]
	assert.Equal(t, rendered.GroupKubelet, kubelet.Group)
	c := kubelet.Contents
	assert.Contains(t, c, "maxPods: 120")
	assert.Contains(t, c, "kubeAPIBurst: 200")
	assert.Contains(t, c, "cgroupsPerQOS: false")
	assert.Contains(t, c, "enforceNodeAllocatable: []")
	assert.Contains(t, c, "- 172.30.0.10")
	assert.Contains(t, c, "containerRuntimeEndpoint: unix:///var/run/containerd/containerd.sock")
	assert.Contains(t, c, "tlsMinVersion: VersionTLS13")
	assert.NotContains(t, c, "cgroupDriver")
}

func TestRenderKubeletRejects(t *testing.T) {
	for name, raw := range map[string]string{
		"cgroupDriver is not allowed": `{"cgroupDriver": "systemd"}`,
		"cgroupsPerQOS cannot be set": `{"cgroupsPerQOS": true}`,
		"podPidsLimit cannot be set":  `{"podPidsLimit": 100}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Render(Inputs{Pool: testPool(), ControllerConfig: testCC(), KubeletConfigs: []*mcfgv1.KubeletConfig{kubeletConfig("x", poolSelector(), raw)}})
			assert.ErrorContains(t, err, name)
		})
	}
}

func TestRenderContainerRuntime(t *testing.T) {
	crc := &mcfgv1.ContainerRuntimeConfig{ObjectMeta: metav1.ObjectMeta{Name: "debug"}, Spec: mcfgv1.ContainerRuntimeConfigSpec{
		MachineConfigPoolSelector: poolSelector(),
		ContainerRuntimeConfig: &mcfgv1.ContainerRuntimeConfiguration{
			LogLevel:   "debug",
			LogSizeMax: ptr.To(resource.MustParse("10Mi")),
			PidsLimit:  ptr.To[int64](2048),
		},
	}}
	res, err := Render(Inputs{Pool: testPool(), ControllerConfig: testCC(), ContainerRuntimeConfigs: []*mcfgv1.ContainerRuntimeConfig{crc}})
	require.NoError(t, err)
	fs := files(t, res)
	assert.Contains(t, fs[ContainerdDropInPath].Contents, `level = "debug"`)
	assert.Contains(t, fs[KubeletConfigPath].Contents, "containerLogMaxSize: 10Mi")
	assert.Contains(t, strings.Join(res.Warnings, "\n"), "pidsLimit has no effect")

	crc.Spec.ContainerRuntimeConfig.LogLevel = "loud"
	_, err = Render(Inputs{Pool: testPool(), ControllerConfig: testCC(), ContainerRuntimeConfigs: []*mcfgv1.ContainerRuntimeConfig{crc}})
	assert.ErrorContains(t, err, "not a containerd log level")
}

func TestRenderIsDeterministic(t *testing.T) {
	in := Inputs{Pool: testPool(), ControllerConfig: testCC()}
	a, err := Render(in)
	require.NoError(t, err)
	b, err := Render(in)
	require.NoError(t, err)
	na, err := a.Config.Name()
	require.NoError(t, err)
	nb, err := b.Config.Name()
	require.NoError(t, err)
	assert.Equal(t, na, nb)
	assert.True(t, strings.HasPrefix(na, "rendered-aix-"))
}

func TestRenderNeedsControllerConfig(t *testing.T) {
	_, err := Render(Inputs{Pool: testPool()})
	assert.ErrorContains(t, err, "ControllerConfig")
}
