package render

import (
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/gilbahat/aix-machine-config-operator/pkg/aix"
	amcov1alpha1 "github.com/gilbahat/aix-machine-config-operator/pkg/apis/amco/v1alpha1"
	"github.com/gilbahat/aix-machine-config-operator/pkg/rendered"
)

// selectMachineConfigs returns the AIXMachineConfigs the pool's
// configSelector selects, in name order. An unset selector selects none.
func selectMachineConfigs(pool *amcov1alpha1.AIXConfigPool, all []*amcov1alpha1.AIXMachineConfig) ([]*amcov1alpha1.AIXMachineConfig, error) {
	if pool.Spec.ConfigSelector == nil {
		return nil, nil
	}
	sel, err := metav1.LabelSelectorAsSelector(pool.Spec.ConfigSelector)
	if err != nil {
		return nil, fmt.Errorf("pool %s: invalid configSelector: %w", pool.Name, err)
	}
	var out []*amcov1alpha1.AIXMachineConfig
	for _, mc := range all {
		if sel.Matches(labels.Set(mc.Labels)) {
			out = append(out, mc)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// mergeMachineConfigs converts and merges configs (already in name order)
// into cfg. For the same item key the later config wins, and every override
// is reported as a warning.
func mergeMachineConfigs(cfg *rendered.Config, configs []*amcov1alpha1.AIXMachineConfig) ([]string, error) {
	var warnings []string
	owner := map[string]string{} // item key -> config that set it
	var (
		files      = map[string]rendered.File{}
		subsystems = map[string]rendered.Subsystem{}
		inittab    = map[string]rendered.InittabEntry{}
		tunables   = map[string]rendered.Tunable{}
		devices    = map[string]rendered.DeviceAttribute{}
	)
	set := func(mc, key string) {
		if prev, ok := owner[key]; ok {
			warnings = append(warnings, fmt.Sprintf("AIXMachineConfig %s overrides %s from %s", mc, key, prev))
		}
		owner[key] = mc
	}
	for _, mc := range configs {
		if mc.Spec.KernelUpdate != nil {
			return nil, fmt.Errorf("AIXMachineConfig %s: kernelUpdate is not yet supported", mc.Name)
		}
		for _, f := range mc.Spec.Files {
			item, err := convertFile(f)
			if err != nil {
				return nil, fmt.Errorf("AIXMachineConfig %s: %w", mc.Name, err)
			}
			if err := validateItem(&rendered.Config{Files: []rendered.File{item}}); err != nil {
				return nil, fmt.Errorf("AIXMachineConfig %s: %w", mc.Name, err)
			}
			set(mc.Name, item.Key())
			files[item.Key()] = item
		}
		for _, s := range mc.Spec.Subsystems {
			item, err := convertSubsystem(s)
			if err != nil {
				return nil, fmt.Errorf("AIXMachineConfig %s: %w", mc.Name, err)
			}
			if err := validateItem(&rendered.Config{Subsystems: []rendered.Subsystem{item}}); err != nil {
				return nil, fmt.Errorf("AIXMachineConfig %s: %w", mc.Name, err)
			}
			set(mc.Name, item.Key())
			subsystems[item.Key()] = item
		}
		for _, e := range mc.Spec.Inittab {
			item := rendered.InittabEntry{
				InittabEntry: aix.InittabEntry{Identifier: e.Identifier, RunLevel: e.RunLevel, Action: e.Action, Command: e.Command},
				Policy:       policy(e.ItemPolicy),
			}
			if err := validateItem(&rendered.Config{Inittab: []rendered.InittabEntry{item}}); err != nil {
				return nil, fmt.Errorf("AIXMachineConfig %s: %w", mc.Name, err)
			}
			set(mc.Name, item.Key())
			inittab[item.Key()] = item
		}
		for _, t := range mc.Spec.Tunables {
			item := rendered.Tunable{Command: t.Command, Name: t.Name, Value: string(t.Value), Policy: policy(t.ItemPolicy)}
			if err := validateItem(&rendered.Config{Tunables: []rendered.Tunable{item}}); err != nil {
				return nil, fmt.Errorf("AIXMachineConfig %s: %w", mc.Name, err)
			}
			set(mc.Name, item.Key())
			tunables[item.Key()] = item
		}
		for _, d := range mc.Spec.DeviceAttributes {
			item := rendered.DeviceAttribute{Device: d.Device, Attribute: d.Attribute, Value: string(d.Value), OnBusy: d.OnBusy, Policy: policy(d.ItemPolicy)}
			if err := validateItem(&rendered.Config{DeviceAttributes: []rendered.DeviceAttribute{item}}); err != nil {
				return nil, fmt.Errorf("AIXMachineConfig %s: %w", mc.Name, err)
			}
			set(mc.Name, item.Key())
			devices[item.Key()] = item
		}
	}
	for _, f := range files {
		cfg.Files = append(cfg.Files, f)
	}
	for _, s := range subsystems {
		cfg.Subsystems = append(cfg.Subsystems, s)
	}
	for _, e := range inittab {
		cfg.Inittab = append(cfg.Inittab, e)
	}
	for _, t := range tunables {
		cfg.Tunables = append(cfg.Tunables, t)
	}
	for _, d := range devices {
		cfg.DeviceAttributes = append(cfg.DeviceAttributes, d)
	}
	return warnings, nil
}

// validateItem runs the agent's own validation on a single item, so errors
// name the AIXMachineConfig they come from.
func validateItem(c *rendered.Config) error {
	c.Version = rendered.FormatVersion
	return c.Validate()
}

func policy(p amcov1alpha1.ItemPolicy) rendered.Policy {
	return rendered.Policy{Drain: p.Drain, Required: p.Required}
}

func convertFile(f amcov1alpha1.FileSpec) (rendered.File, error) {
	out := rendered.File{Path: f.Path, Mode: 0o644, Owner: f.Owner, Group: f.Group, Origin: rendered.OriginUser, Policy: policy(f.ItemPolicy)}
	switch {
	case f.Contents.Inline != nil && f.Contents.Base64 == nil:
		out.Contents = *f.Contents.Inline
	case f.Contents.Base64 != nil && f.Contents.Inline == nil:
		out.Contents, out.Encoding = *f.Contents.Base64, "base64"
	default:
		return out, fmt.Errorf("file %s: set exactly one of contents.inline and contents.base64", f.Path)
	}
	if f.Mode != nil {
		if *f.Mode < 0 || *f.Mode > 0o777 {
			return out, fmt.Errorf("file %s: mode %o must be permission bits only (0 to 0777)", f.Path, *f.Mode)
		}
		out.Mode = uint32(*f.Mode)
	}
	for _, a := range f.OnChange {
		action, ok := map[string]string{"Restart": rendered.ActionRestart, "Refresh": rendered.ActionRefresh}[a.Action]
		if !ok {
			return out, fmt.Errorf("file %s: onChange action %q must be Restart or Refresh", f.Path, a.Action)
		}
		out.Actions = append(out.Actions, rendered.SubsystemAction{Subsystem: a.Subsystem, Action: action})
	}
	return out, nil
}

func convertSubsystem(s amcov1alpha1.SubsystemSpec) (rendered.Subsystem, error) {
	out := rendered.Subsystem{Name: s.Name, Policy: policy(s.ItemPolicy)}
	switch s.State {
	case "":
	case "Active":
		out.State = rendered.SubsystemActive
	case "Inoperative":
		out.State = rendered.SubsystemInoperative
	default:
		return out, fmt.Errorf("subsystem %s: state %q must be Active or Inoperative", s.Name, s.State)
	}
	if d := s.Definition; d != nil {
		def := aix.SubsystemDefinition{
			Path: d.Path, Arguments: d.Arguments, Stdin: d.Stdin, Stdout: d.Stdout, Stderr: d.Stderr,
			Respawn: d.Respawn, Multiple: d.MultipleInstances,
			StopSignal: int(d.StopSignal), ForceSignal: int(d.ForceSignal), WaitTime: int(d.WaitSeconds), Group: d.Group,
		}
		if d.UID != nil {
			def.UID = int(*d.UID)
		}
		switch d.Contact {
		case "", "Signal":
			def.Contact = aix.ContactSignal
		case "Socket":
			def.Contact = aix.ContactSocket
		default:
			return out, fmt.Errorf("subsystem %s: contact %q must be Signal or Socket", s.Name, d.Contact)
		}
		def = def.WithDefaults()
		out.Definition = &def
	}
	return out, nil
}
