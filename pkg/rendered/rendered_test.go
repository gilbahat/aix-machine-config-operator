package rendered

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cfg(files ...File) *Config {
	return &Config{Version: FormatVersion, Pool: "aix", Files: files}
}

var (
	hosts   = File{Path: "/etc/containerd/certs.d/quay.io/hosts.toml", Mode: 0o644, Contents: "a", Group: GroupRegistries}
	kubelet = File{Path: "/etc/kubernetes/kubelet.conf", Mode: 0o644, Contents: "k", Group: GroupKubelet}
	trust   = File{Path: "/var/ssl/certs/amco.pem", Mode: 0o644, Contents: "t", Group: GroupTrust}
)

func TestDiffRegistriesOnlyIsLive(t *testing.T) {
	changed := hosts
	changed.Contents = "b"
	p := Diff(cfg(hosts, kubelet), cfg(changed, kubelet))
	assert.Equal(t, []File{changed}, p.Write)
	assert.False(t, p.Drain)
	assert.Empty(t, p.Restart)
}

func TestDiffKubeletDrains(t *testing.T) {
	changed := kubelet
	changed.Contents = "k2"
	p := Diff(cfg(hosts, kubelet, trust), cfg(hosts, changed))
	assert.True(t, p.Drain)
	assert.Equal(t, []string{"containerd", "kubelet"}, p.Restart)
	assert.Equal(t, []string{trust.Path}, p.Remove)
}

func TestDiffFromNothing(t *testing.T) {
	p := Diff(nil, cfg(hosts, kubelet))
	assert.Len(t, p.Write, 2)
	assert.True(t, p.Drain)
	assert.True(t, Diff(cfg(hosts), cfg(hosts)).Empty())
}

func TestValidate(t *testing.T) {
	for _, f := range []File{
		{Path: "relative", Group: GroupRegistries},
		{Path: "/etc/../etc/x", Group: GroupRegistries},
		{Path: "/x", Group: "nope"},
		{Path: "/x", Mode: 0o10000, Group: GroupRegistries},
	} {
		assert.Error(t, cfg(f).Validate(), f.Path)
	}
	assert.Error(t, cfg(hosts, hosts).Validate())
	assert.Error(t, (&Config{Version: 99}).Validate())
}

func TestRoundTripAndName(t *testing.T) {
	c := cfg(kubelet, hosts)
	data, err := c.Marshal()
	require.NoError(t, err)
	back, err := Unmarshal(data)
	require.NoError(t, err)
	assert.Equal(t, c, back)
	n1, _ := cfg(kubelet, hosts).Name()
	n2, _ := cfg(hosts, kubelet).Name()
	assert.Equal(t, n1, n2)
	assert.Regexp(t, `^rendered-aix-[0-9a-f]{16}$`, n1)
}
