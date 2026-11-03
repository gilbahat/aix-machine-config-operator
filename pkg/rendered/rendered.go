// Package rendered defines the configuration AMCO renders for a pool of AIX
// nodes and the rules that decide how disruptive applying it is. The operator
// produces a Config and stores it in an immutable ConfigMap named after the
// Config's content hash; the node agent reads it back and applies it. The
// package is deliberately free of heavy dependencies so the agent can share it.
package rendered

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
)

// FormatVersion is the version of the Config schema.
const FormatVersion = 1

// ConfigMapKey is the key of the serialized Config inside its ConfigMap.
const ConfigMapKey = "config.json"

// PoolLabel labels rendered ConfigMaps with the pool they were rendered for.
const PoolLabel = "amco.reuna-labs.io/pool"

// Group classifies a file by what must happen on the node when it changes.
type Group string

// File groups.
const (
	// GroupRegistries covers containerd's registry host configuration
	// (/etc/containerd/certs.d). containerd reads it on every pull.
	GroupRegistries Group = "registries"
	// GroupKubeletCA covers the kubelet client CA bundle, which the kubelet
	// reloads when the file changes.
	GroupKubeletCA Group = "kubelet-ca"
	// GroupTrust covers additional system trust anchors. Go programs read
	// the system roots once, so containerd is restarted to pick them up.
	GroupTrust Group = "trust"
	// GroupContainerd covers containerd configuration drop-ins.
	GroupContainerd Group = "containerd"
	// GroupKubelet covers the kubelet configuration file. Changing it means
	// restarting the kubelet, so the node is drained first.
	GroupKubelet Group = "kubelet"
)

// Policy is what applying a change to a group requires.
type Policy struct {
	// Drain means the node is cordoned and drained before the change is applied.
	Drain bool
	// Restart lists SRC subsystems restarted after the files are written.
	Restart []string
}

// SRC subsystem names of the node services AMCO restarts.
const (
	SubsystemContainerd = "containerd"
	SubsystemKubelet    = "kubelet"
)

// Policies maps each group to its disruption policy. They follow the stock
// MCO's defaults: registry and CA changes are applied live, kubelet
// configuration changes drain the node.
var Policies = map[Group]Policy{
	GroupRegistries: {},
	GroupKubeletCA:  {},
	GroupTrust:      {Restart: []string{SubsystemContainerd}},
	GroupContainerd: {Restart: []string{SubsystemContainerd}},
	GroupKubelet:    {Drain: true, Restart: []string{SubsystemKubelet}},
}

// File is one file AMCO owns on the node.
type File struct {
	// Path is absolute.
	Path string `json:"path"`
	// Mode holds the permission bits.
	Mode uint32 `json:"mode"`
	// Contents is the full file content.
	Contents string `json:"contents"`
	// Group decides what applying a change to the file requires.
	Group Group `json:"group"`
}

// Config is the full configuration rendered for a pool.
type Config struct {
	Version int    `json:"version"`
	Pool    string `json:"pool"`
	Files   []File `json:"files"`
}

// managedFiles and managedPrefixes are the only places a rendered config may
// write. The agent runs as root, so this list (not the operator) is what
// bounds what a rendered config can do to a node.
var (
	managedFiles = map[string]bool{
		"/etc/kubernetes/kubelet.conf":   true,
		"/etc/kubernetes/kubelet-ca.crt": true,
	}
	managedPrefixes = []string{
		"/etc/containerd/certs.d/",
		"/etc/containerd/conf.d/",
		"/var/ssl/certs/amco-",
	}
)

// IsManagedPath reports whether a rendered config may write p.
func IsManagedPath(p string) bool {
	if managedFiles[p] {
		return true
	}
	for _, pre := range managedPrefixes {
		if strings.HasPrefix(p, pre) && len(p) > len(pre) {
			return true
		}
	}
	return false
}

// Validate checks the structural rules the agent relies on.
func (c *Config) Validate() error {
	if c.Version != FormatVersion {
		return fmt.Errorf("unsupported rendered config version %d (agent supports %d)", c.Version, FormatVersion)
	}
	seen := map[string]bool{}
	for _, f := range c.Files {
		if !path.IsAbs(f.Path) || path.Clean(f.Path) != f.Path || f.Path == "/" {
			return fmt.Errorf("file path %q must be absolute and clean", f.Path)
		}
		if !IsManagedPath(f.Path) {
			return fmt.Errorf("file path %q is outside the locations AMCO manages", f.Path)
		}
		if seen[f.Path] {
			return fmt.Errorf("file %q listed twice", f.Path)
		}
		seen[f.Path] = true
		if f.Mode&^0o777 != 0 {
			return fmt.Errorf("file %q has invalid mode %o (only permission bits are allowed)", f.Path, f.Mode)
		}
		if _, ok := Policies[f.Group]; !ok {
			return fmt.Errorf("file %q has unknown group %q", f.Path, f.Group)
		}
	}
	return nil
}

// Normalize sorts the files by path so equal configs serialize identically.
func (c *Config) Normalize() {
	sort.Slice(c.Files, func(i, j int) bool { return c.Files[i].Path < c.Files[j].Path })
}

// Marshal returns the canonical JSON encoding of the config.
func (c *Config) Marshal() ([]byte, error) {
	c.Normalize()
	return json.Marshal(c)
}

// Unmarshal decodes and validates a config.
func Unmarshal(data []byte) (*Config, error) {
	c := &Config{}
	if err := json.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("decoding rendered config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	c.Normalize()
	return c, nil
}

// Name returns the content-addressed name of the config, which is also the
// name of its ConfigMap: rendered-<pool>-<first 16 hex digits of sha256>.
func (c *Config) Name() (string, error) {
	data, err := c.Marshal()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("rendered-%s-%s", c.Pool, hex.EncodeToString(sum[:8])), nil
}

// Plan is what moving a node from one config to another requires.
type Plan struct {
	// Write lists files to create or overwrite.
	Write []File
	// Remove lists paths of files that are no longer managed.
	Remove []string
	// Drain is true when the node must be drained first.
	Drain bool
	// Restart lists SRC subsystems to restart afterwards, in a stable order.
	Restart []string
}

// Empty reports whether the plan changes nothing.
func (p Plan) Empty() bool { return len(p.Write) == 0 && len(p.Remove) == 0 }

// Diff computes the plan that turns from into to. A nil from means nothing is
// known about the node, so every file is written and every policy of a group
// present in to applies.
func Diff(from, to *Config) Plan {
	var p Plan
	groups := map[Group]bool{}
	old := map[string]File{}
	if from != nil {
		for _, f := range from.Files {
			old[f.Path] = f
		}
	}
	for _, f := range to.Files {
		if o, ok := old[f.Path]; ok && o == f {
			continue
		}
		p.Write = append(p.Write, f)
		groups[f.Group] = true
		if o, ok := old[f.Path]; ok {
			groups[o.Group] = true
		}
	}
	newPaths := map[string]bool{}
	for _, f := range to.Files {
		newPaths[f.Path] = true
	}
	if from != nil {
		for _, f := range from.Files {
			if !newPaths[f.Path] {
				p.Remove = append(p.Remove, f.Path)
				groups[f.Group] = true
			}
		}
	}
	p.Drain, p.Restart = PolicyFor(groups)
	sort.Strings(p.Remove)
	return p
}

// PolicyFor combines the policies of the changed groups: whether a drain is
// needed and which subsystems to restart, in restart order.
func PolicyFor(groups map[Group]bool) (drain bool, restartOrder []string) {
	restart := map[string]bool{}
	for g := range groups {
		pol := Policies[g]
		drain = drain || pol.Drain
		for _, s := range pol.Restart {
			restart[s] = true
		}
	}
	// containerd before the kubelet, so the kubelet reconnects to a running runtime.
	for _, s := range []string{SubsystemContainerd, SubsystemKubelet} {
		if restart[s] {
			restartOrder = append(restartOrder, s)
			delete(restart, s)
		}
	}
	var rest []string
	for s := range restart {
		rest = append(rest, s)
	}
	sort.Strings(rest)
	return drain, append(restartOrder, rest...)
}

// String summarizes the plan for logs and events.
func (p Plan) String() string {
	var parts []string
	parts = append(parts, fmt.Sprintf("write %d", len(p.Write)), fmt.Sprintf("remove %d", len(p.Remove)))
	if p.Drain {
		parts = append(parts, "drain")
	}
	if len(p.Restart) > 0 {
		parts = append(parts, "restart "+strings.Join(p.Restart, ","))
	}
	return strings.Join(parts, ", ")
}
