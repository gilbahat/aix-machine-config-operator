package rendered

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gilbahat/aix-machine-config-operator/pkg/aix"
)

func cfg(files ...File) *Config {
	return &Config{Version: FormatVersion, Pool: "aix", Files: files}
}

var (
	hosts   = File{Path: "/etc/containerd/certs.d/quay.io/hosts.toml", Mode: 0o644, Contents: "a", Origin: OriginAMCO}
	kubelet = File{Path: "/etc/kubernetes/kubelet.conf", Mode: 0o644, Contents: "k", Origin: OriginAMCO,
		Actions: []SubsystemAction{{Subsystem: SubsystemKubelet, Action: ActionRestart}}, Policy: Policy{Drain: true}}
	sshd = File{Path: "/etc/ssh/sshd_config", Mode: 0o644, Contents: "PermitRootLogin no\n", Origin: OriginUser,
		Actions: []SubsystemAction{{Subsystem: "sshd", Action: ActionRefresh}}}
)

func TestChangesAndDrain(t *testing.T) {
	changed := hosts
	changed.Contents = "b"
	assert.Equal(t, []string{FileKey(hosts.Path)}, Changes(cfg(hosts, kubelet), cfg(changed, kubelet)))
	assert.False(t, DrainNeeded(cfg(hosts, kubelet), cfg(changed, kubelet)))

	k2 := kubelet
	k2.Contents = "k2"
	assert.True(t, DrainNeeded(cfg(hosts, kubelet), cfg(hosts, k2)))
	assert.True(t, DrainNeeded(cfg(hosts, kubelet), cfg(hosts)), "removing a drain item drains too")
	assert.True(t, DrainNeeded(nil, cfg(hosts, kubelet)))
	assert.Empty(t, Changes(cfg(hosts), cfg(hosts)))

	withTunable := cfg(hosts)
	withTunable.Tunables = []Tunable{{Command: "no", Name: "tcp_sendspace", Value: "262144"}}
	assert.Equal(t, []string{TunableKey("no", "tcp_sendspace")}, Changes(cfg(hosts), withTunable))
	assert.False(t, DrainNeeded(cfg(hosts), withTunable))
	withTunable.Tunables[0].Drain = true
	assert.True(t, DrainNeeded(cfg(hosts), withTunable))
}

func TestValidateFiles(t *testing.T) {
	require.NoError(t, cfg(hosts, kubelet, sshd).Validate())
	user := func(p string) File { return File{Path: p, Mode: 0o644, Origin: OriginUser} }
	for _, f := range []File{
		{Path: "relative", Origin: OriginUser},
		{Path: "/etc/../etc/x", Origin: OriginUser},
		{Path: "/etc/containerd/certs.d/q/hosts.toml", Origin: "nope"},
		{Path: "/etc/containerd/certs.d/q/hosts.toml", Mode: 0o4755, Origin: OriginAMCO},
		{Path: "/etc/amco/kubeconfig", Origin: OriginAMCO},
		{Path: "/etc/containerd/hosts.toml", Origin: OriginAMCO},
		user("/etc/kubernetes/kubelet.conf"),
		user("/etc/containerd/certs.d/quay.io/hosts.toml"),
		user("/etc/amco/applied-config.json"),
		user("/etc/amco"),
		user("/etc/inittab"),
		user("/etc/tunables/nextboot"),
		user("/opt/amco/bin/amco-agent"),
		user("/var/ssl/certs/amco-x.pem"),
		user("/etc/motd.amco-orig"),
		{Path: "/etc/motd", Origin: OriginUser, Encoding: "base64", Contents: "not base64!"},
		{Path: "/etc/motd", Origin: OriginUser, Owner: "root; rm"},
		{Path: "/etc/motd", Origin: OriginUser, Actions: []SubsystemAction{{Subsystem: "sshd", Action: "kill"}}},
		{Path: "/etc/motd", Origin: OriginUser, Actions: []SubsystemAction{{Subsystem: SubsystemAgent, Action: ActionRestart}}},
	} {
		assert.Error(t, cfg(f).Validate(), "%+v", f)
	}
	assert.Error(t, cfg(hosts, hosts).Validate())
	assert.Error(t, (&Config{Version: 1}).Validate())

	ok := File{Path: "/etc/motd", Origin: OriginUser, Owner: "bin", Group: "3", Encoding: "base64", Contents: "aGVsbG8K"}
	require.NoError(t, cfg(ok).Validate())
	data, err := ok.Data()
	require.NoError(t, err)
	assert.Equal(t, "hello\n", string(data))
}

func TestValidateOtherItems(t *testing.T) {
	valid := &Config{Version: FormatVersion, Pool: "aix",
		Subsystems:       []Subsystem{{Name: "sshd", State: SubsystemActive}, {Name: "appd", Definition: &aix.SubsystemDefinition{Path: "/opt/app/appd"}}},
		Inittab:          []InittabEntry{{InittabEntry: aix.InittabEntry{Identifier: "appd", RunLevel: "2", Action: "once", Command: "/usr/bin/startsrc -s appd"}}},
		Tunables:         []Tunable{{Command: "no", Name: "tcp_sendspace", Value: "262144"}},
		DeviceAttributes: []DeviceAttribute{{Device: "fcs0", Attribute: "num_cmd_elems", Value: "2048"}},
	}
	require.NoError(t, valid.Validate())

	for name, mutate := range map[string]func(c *Config){
		"agent subsystem":   func(c *Config) { c.Subsystems[0].Name = SubsystemAgent },
		"bad state":         func(c *Config) { c.Subsystems[0].State = "running" },
		"empty subsystem":   func(c *Config) { c.Subsystems[0].Definition = nil }, // appd sorts first
		"agent inittab":     func(c *Config) { c.Inittab[0].Identifier = "amco-agent" },
		"bad tunable cmd":   func(c *Config) { c.Tunables[0].Command = "sh" },
		"bad tunable value": func(c *Config) { c.Tunables[0].Value = "1 2" },
		"bad device":        func(c *Config) { c.DeviceAttributes[0].Device = "-P" },
		"bad onBusy":        func(c *Config) { c.DeviceAttributes[0].OnBusy = "Wait" },
		"duplicate tunable": func(c *Config) { c.Tunables = append(c.Tunables, c.Tunables[0]) },
	} {
		c, err := Unmarshal(mustMarshal(t, valid))
		require.NoError(t, err)
		mutate(c)
		assert.Error(t, c.Validate(), name)
	}
}

func mustMarshal(t *testing.T, c *Config) []byte {
	data, err := c.Marshal()
	require.NoError(t, err)
	return data
}

func TestRoundTripAndName(t *testing.T) {
	c := cfg(kubelet, hosts, sshd)
	c.Tunables = []Tunable{{Command: "vmo", Name: "minfree", Value: "960"}, {Command: "no", Name: "tcp_sendspace", Value: "1"}}
	back, err := Unmarshal(mustMarshal(t, c))
	require.NoError(t, err)
	assert.Equal(t, c, back)
	n1, _ := cfg(kubelet, hosts).Name()
	n2, _ := cfg(hosts, kubelet).Name()
	assert.Equal(t, n1, n2)
	assert.Regexp(t, `^rendered-aix-[0-9a-f]{16}$`, n1)
}
