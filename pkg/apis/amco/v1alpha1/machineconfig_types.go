package v1alpha1

import (
	"bytes"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ItemValue is a tunable or device attribute value, written as a YAML number
// or string. Unlike intstr.IntOrString it keeps numbers of any size, so a
// 64-bit tunable value cannot break decoding.
type ItemValue string

// UnmarshalJSON accepts a JSON string or number.
func (v *ItemValue) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*v = ItemValue(s)
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var n json.Number
	if err := dec.Decode(&n); err != nil {
		return fmt.Errorf("value must be a number or a string: %w", err)
	}
	*v = ItemValue(n.String())
	return nil
}

// MarshalJSON writes the value as a string, which int-or-string accepts.
func (v ItemValue) MarshalJSON() ([]byte, error) { return json.Marshal(string(v)) }

// AIXMachineConfig declares host configuration for AIX nodes: files, SRC
// subsystems, inittab entries, kernel tunables and device attributes. Pools
// select configs with spec.configSelector, as MachineConfigPools select
// MachineConfigs.
//
// The agent converges every item in place: it reads the live state, changes
// only what differs and verifies the result. It never reboots the node.
// Changes that only take effect at the next boot are applied persistently
// and reported as pending; changes that cannot converge in place are
// reported as needing reprovisioning.
//
// Creating an AIXMachineConfig is equivalent to cluster-admin on every AIX
// node: subsystem definitions and inittab entries run arbitrary programs as
// root.
type AIXMachineConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec AIXMachineConfigSpec `json:"spec,omitempty"`
}

// AIXMachineConfigSpec is the host configuration. Configs selected by a pool
// are merged in name order; for the same item, the later config wins.
type AIXMachineConfigSpec struct {
	Files            []FileSpec            `json:"files,omitempty"`
	Subsystems       []SubsystemSpec       `json:"subsystems,omitempty"`
	Inittab          []InittabSpec         `json:"inittab,omitempty"`
	Tunables         []TunableSpec         `json:"tunables,omitempty"`
	DeviceAttributes []DeviceAttributeSpec `json:"deviceAttributes,omitempty"`

	// KernelUpdate is reserved for operator-coordinated kernel updates
	// (preferring Live Kernel Update). It is not yet supported and any
	// value is rejected.
	// +optional
	KernelUpdate *runtime.RawExtension `json:"kernelUpdate,omitempty"`
}

// ItemPolicy holds the settings every item has.
type ItemPolicy struct {
	// Drain cordons and drains the node before this item is added, changed
	// or removed. Restarting containerd or the kubelet does not need it.
	// +optional
	Drain bool `json:"drain,omitempty"`
	// Required makes the node Degraded, and blocks the rest of the config,
	// when the item cannot converge in place. Otherwise such an item is
	// skipped and reported as needing reprovisioning.
	// +optional
	Required bool `json:"required,omitempty"`
}

// FileSpec is a file on the node.
type FileSpec struct {
	// Path is absolute. Paths AMCO renders itself, /etc/amco, /opt/amco,
	// /etc/tunables and /etc/inittab are reserved.
	Path string `json:"path"`
	// Contents is the file content.
	Contents FileContents `json:"contents"`
	// Mode holds the permission bits; defaults to 0644.
	// +optional
	Mode *int32 `json:"mode,omitempty"`
	// Owner is a user name or numeric uid; defaults to root.
	// +optional
	Owner string `json:"owner,omitempty"`
	// Group is a group name or numeric gid; defaults to system (0).
	// +optional
	Group string `json:"group,omitempty"`
	// OnChange lists subsystems to restart or refresh after the file changes.
	// +optional
	OnChange   []SubsystemActionSpec `json:"onChange,omitempty"`
	ItemPolicy `json:",inline"`
}

// FileContents holds a file's content, inline or base64-encoded. Exactly
// one of the fields is set.
type FileContents struct {
	// +optional
	Inline *string `json:"inline,omitempty"`
	// +optional
	Base64 *string `json:"base64,omitempty"`
}

// SubsystemActionSpec is an SRC action triggered by a file change.
type SubsystemActionSpec struct {
	Subsystem string `json:"subsystem"`
	// Action is Restart or Refresh (refresh, or SIGHUP for subsystems that
	// only use signals).
	Action string `json:"action"`
}

// SubsystemSpec is an SRC subsystem.
type SubsystemSpec struct {
	Name string `json:"name"`
	// State is Active or Inoperative. Leave it unset to manage only the
	// definition.
	// +optional
	State string `json:"state,omitempty"`
	// Definition, when set, creates the subsystem (mkssys) or keeps its
	// definition matching (chssys).
	// +optional
	Definition *SubsystemDefinitionSpec `json:"definition,omitempty"`
	ItemPolicy `json:",inline"`
}

// SubsystemDefinitionSpec maps to mkssys/chssys flags.
type SubsystemDefinitionSpec struct {
	// Path is the program (-p).
	Path string `json:"path"`
	// Arguments are passed to the program (-a).
	// +optional
	Arguments string `json:"arguments,omitempty"`
	// UID runs the program as this user id (-u); defaults to 0.
	// +optional
	UID *int32 `json:"uid,omitempty"`
	// Stdin, Stdout and Stderr default to /dev/console (-i, -o, -e).
	// +optional
	Stdin string `json:"stdin,omitempty"`
	// +optional
	Stdout string `json:"stdout,omitempty"`
	// +optional
	Stderr string `json:"stderr,omitempty"`
	// Respawn restarts the program when it ends abnormally (-R; -O otherwise).
	// +optional
	Respawn bool `json:"respawn,omitempty"`
	// MultipleInstances allows several instances (-q; -Q otherwise).
	// +optional
	MultipleInstances bool `json:"multipleInstances,omitempty"`
	// Contact is Signal (-S, default) or Socket (-K).
	// +optional
	Contact string `json:"contact,omitempty"`
	// StopSignal and ForceSignal are used with Signal contact (-n, -f);
	// they default to 15 and 9.
	// +optional
	StopSignal int32 `json:"stopSignal,omitempty"`
	// +optional
	ForceSignal int32 `json:"forceSignal,omitempty"`
	// WaitSeconds is how long a stop may take before it is forced (-w);
	// defaults to 20.
	// +optional
	WaitSeconds int32 `json:"waitSeconds,omitempty"`
	// Group is the SRC group (-G).
	// +optional
	Group string `json:"group,omitempty"`
}

// InittabSpec is one /etc/inittab entry, managed with mkitab, chitab and
// rmitab. No other entry is touched.
type InittabSpec struct {
	Identifier string `json:"identifier"`
	RunLevel   string `json:"runLevel"`
	Action     string `json:"action"`
	Command    string `json:"command"`
	ItemPolicy `json:",inline"`
}

// TunableSpec is a kernel tunable set with no, vmo, ioo, schedo, nfso or raso.
type TunableSpec struct {
	Command    string    `json:"command"`
	Name       string    `json:"name"`
	Value      ItemValue `json:"value"`
	ItemPolicy `json:",inline"`
}

// DeviceAttributeSpec is a device attribute set with chdev.
type DeviceAttributeSpec struct {
	Device    string    `json:"device"`
	Attribute string    `json:"attribute"`
	Value     ItemValue `json:"value"`
	// OnBusy is what happens when the device is in use: Defer (default)
	// records the change for the next boot (chdev -P) and reports it as
	// pending; Fail makes the node Degraded.
	// +optional
	OnBusy     string `json:"onBusy,omitempty"`
	ItemPolicy `json:",inline"`
}

// AIXMachineConfigList is a list of AIXMachineConfigs.
type AIXMachineConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []AIXMachineConfig `json:"items"`
}

// MachineConfigResource is the plural resource name of AIXMachineConfig.
const MachineConfigResource = "aixmachineconfigs"

// RoleLabel is the label default pools select configs by, as
// machineconfiguration.openshift.io/role does for MachineConfigs.
const RoleLabel = GroupName + "/role"
