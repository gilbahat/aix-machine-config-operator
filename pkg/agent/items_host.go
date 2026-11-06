package agent

import (
	"errors"
	"fmt"

	"k8s.io/klog/v2"

	"github.com/gilbahat/aix-machine-config-operator/pkg/aix"
	"github.com/gilbahat/aix-machine-config-operator/pkg/rendered"
)

// SRC subsystems.

func (r *run) checkSubsystem(s rendered.Subsystem) (status, string, error) {
	if r.src == nil {
		return inSync, "", errNoRunner
	}
	if s.Definition != nil {
		live, err := r.src.Definition(r.ctx, s.Name)
		if errors.Is(err, aix.ErrSubsystemNotFound) {
			return needsChange, "not defined", nil
		}
		if err != nil {
			return inSync, "", err
		}
		if live != s.Definition.WithDefaults() {
			return needsChange, "definition differs", nil
		}
	}
	if s.State == "" {
		return inSync, "", nil
	}
	st, err := r.src.Status(r.ctx, s.Name)
	if errors.Is(err, aix.ErrSubsystemNotFound) {
		if s.Definition == nil {
			return inSync, "", reprovision("subsystem %s is not defined on this node and the config gives no definition", s.Name)
		}
		return needsChange, "not defined", nil
	}
	if err != nil {
		return inSync, "", err
	}
	if st.State != s.State {
		return needsChange, fmt.Sprintf("%s, want %s", st.State, s.State), nil
	}
	return inSync, "", nil
}

func (r *run) baselineSubsystem(s rendered.Subsystem) (Baseline, error) {
	b := &SubsystemBaseline{}
	def, err := r.src.Definition(r.ctx, s.Name)
	if errors.Is(err, aix.ErrSubsystemNotFound) {
		return Baseline{Subsystem: b}, nil
	}
	if err != nil {
		return Baseline{}, err
	}
	st, err := r.src.Status(r.ctx, s.Name)
	if err != nil {
		return Baseline{}, err
	}
	b.Defined, b.Definition, b.State = true, &def, st.State
	return Baseline{Subsystem: b}, nil
}

func (r *run) applySubsystem(s rendered.Subsystem) error {
	if s.Definition != nil {
		live, err := r.src.Definition(r.ctx, s.Name)
		switch {
		case errors.Is(err, aix.ErrSubsystemNotFound):
			if err := r.src.Define(r.ctx, s.Name, *s.Definition); err != nil {
				return err
			}
			klog.Infof("defined SRC subsystem %s", s.Name)
		case err != nil:
			return err
		case live != s.Definition.WithDefaults():
			if err := r.src.Redefine(r.ctx, s.Name, *s.Definition); err != nil {
				return err
			}
			klog.Infof("changed the definition of SRC subsystem %s", s.Name)
			// A running subsystem keeps its old definition until restarted.
			r.actions[s.Name] = rendered.SubsystemAction{Subsystem: s.Name, Action: rendered.ActionRestart}
		}
	}
	return r.convergeState(s.Name, s.State)
}

func (r *run) convergeState(name, want string) error {
	if want == "" {
		return nil
	}
	st, err := r.src.Status(r.ctx, name)
	if err != nil {
		return err
	}
	switch {
	case want == rendered.SubsystemActive && st.State != aix.StateActive:
		if err := r.src.Start(r.ctx, name); err != nil {
			return err
		}
		r.started[name] = true
		klog.Infof("started SRC subsystem %s", name)
	case want == rendered.SubsystemInoperative && st.State != aix.StateInoperative:
		if err := r.src.Stop(r.ctx, name); err != nil {
			return err
		}
		klog.Infof("stopped SRC subsystem %s", name)
	}
	return nil
}

func (r *run) restoreSubsystem(name string, b *SubsystemBaseline) error {
	if r.src == nil {
		return errNoRunner
	}
	if b == nil {
		return nil
	}
	if !b.Defined {
		if _, err := r.src.Definition(r.ctx, name); errors.Is(err, aix.ErrSubsystemNotFound) {
			return nil
		}
		if err := r.src.Stop(r.ctx, name); err != nil {
			return err
		}
		klog.Infof("removing SRC subsystem %s (AMCO defined it)", name)
		return r.src.Undefine(r.ctx, name)
	}
	if b.Definition != nil {
		live, err := r.src.Definition(r.ctx, name)
		if err != nil {
			return err
		}
		if live != *b.Definition {
			if err := r.src.Redefine(r.ctx, name, *b.Definition); err != nil {
				return err
			}
			r.actions[name] = rendered.SubsystemAction{Subsystem: name, Action: rendered.ActionRestart}
		}
	}
	switch b.State {
	case aix.StateActive:
		return r.convergeState(name, rendered.SubsystemActive)
	case aix.StateInoperative:
		return r.convergeState(name, rendered.SubsystemInoperative)
	}
	return nil
}

// Inittab entries.

func (r *run) checkInittab(e rendered.InittabEntry) (status, string, error) {
	if r.itab.Runner == nil {
		return inSync, "", errNoRunner
	}
	live, err := r.itab.Get(r.ctx, e.Identifier)
	if errors.Is(err, aix.ErrInittabEntryNotFound) {
		return needsChange, "missing", nil
	}
	if err != nil {
		return inSync, "", err
	}
	if live != e.InittabEntry {
		return needsChange, fmt.Sprintf("is %q", live.String()), nil
	}
	return inSync, "", nil
}

func (r *run) baselineInittab(e rendered.InittabEntry) (Baseline, error) {
	live, err := r.itab.Get(r.ctx, e.Identifier)
	if errors.Is(err, aix.ErrInittabEntryNotFound) {
		return Baseline{Inittab: &InittabBaseline{}}, nil
	}
	if err != nil {
		return Baseline{}, err
	}
	return Baseline{Inittab: &InittabBaseline{Entry: &live}}, nil
}

// Tunables.

// tunableInfo reads a tunable and rejects the ones AMCO cannot set.
func (r *run) tunableInfo(command, name string) (aix.TunableInfo, error) {
	if r.tun == nil {
		return aix.TunableInfo{}, errNoRunner
	}
	info, err := r.tun.Get(r.ctx, command, name)
	if err != nil {
		return info, err
	}
	if info.Restricted {
		return info, fmt.Errorf("%s %s is a restricted tunable; AMCO does not change restricted tunables", command, name)
	}
	switch info.Type {
	case aix.TunableDynamic, aix.TunableMount, aix.TunableConnect, aix.TunableIncremental, aix.TunableReboot, aix.TunableBosboot:
	case aix.TunableStatic:
		return info, fmt.Errorf("%s %s is a Static tunable and can never be changed", command, name)
	case aix.TunableDeprecated:
		return info, fmt.Errorf("%s %s is deprecated and cannot be changed", command, name)
	default:
		return info, fmt.Errorf("%s %s has unknown tunable type %q", command, name, info.Type)
	}
	return info, nil
}

// atBoot reports whether setting value needs -r (next boot only): Reboot
// and Bosboot tunables, and lowering an Incremental one.
func atBoot(info aix.TunableInfo, value string) bool {
	switch info.Type {
	case aix.TunableReboot, aix.TunableBosboot:
		return true
	case aix.TunableIncremental:
		cur, okC := aix.ParseTunableValue(info.Current)
		want, okW := aix.ParseTunableValue(value)
		return okC && okW && want.Cmp(cur) < 0
	}
	return false
}

func (r *run) checkTunable(t rendered.Tunable) (status, string, error) {
	return r.checkTunableValue(t.Command, t.Name, t.Value, t.Key())
}

func (r *run) checkTunableValue(command, name, value, key string) (status, string, error) {
	info, err := r.tunableInfo(command, name)
	if err != nil {
		return inSync, "", err
	}
	cur := aix.TunableValuesEqual(info.Current, value)
	boot := aix.TunableValuesEqual(info.Boot, value)
	switch {
	case cur && boot:
		return inSync, "", nil
	case boot && atBoot(info, value) && r.pendingThisBoot(key, value):
		return awaitingReboot, "", nil
	}
	return needsChange, fmt.Sprintf("current %s, next boot %s, want %s", info.Current, info.Boot, value), nil
}

func (r *run) baselineTunable(t rendered.Tunable) (Baseline, error) {
	info, err := r.tunableInfo(t.Command, t.Name)
	if err != nil {
		return Baseline{}, err
	}
	return Baseline{Tunable: &ValueBaseline{Current: info.Current, Boot: info.Boot}}, nil
}

func (r *run) applyTunable(command, name, value, key string) error {
	info, err := r.tunableInfo(command, name)
	if err != nil {
		return err
	}
	if !atBoot(info, value) {
		delete(r.st.PendingReboot, key)
		return r.tun.SetNow(r.ctx, command, name, value)
	}
	if err := r.tun.SetReboot(r.ctx, command, name, value); err != nil {
		return err
	}
	r.markPending(key, value)
	if info.Type == aix.TunableBosboot {
		r.bosboot = true
	}
	klog.Infof("%s %s=%s takes effect at the next boot", command, name, value)
	return nil
}

func (r *run) restoreTunable(t rendered.Tunable, b *ValueBaseline) error {
	if b == nil {
		return nil
	}
	// Restore the next-boot value, the one that persists; for dynamic
	// tunables that also sets the running value.
	value, ok := decimal(b.Boot)
	if !ok {
		value, ok = decimal(b.Current)
	}
	if !ok {
		klog.Warningf("cannot restore %s: its original value %q/%q is not a number", t.Key(), b.Current, b.Boot)
		return nil
	}
	s, _, err := r.checkTunableValue(t.Command, t.Name, value, t.Key())
	if err != nil || s != needsChange {
		if s == inSync {
			delete(r.st.PendingReboot, t.Key())
		}
		return err
	}
	return r.applyTunable(t.Command, t.Name, value, t.Key())
}

// decimal turns a listed value (possibly abbreviated, like 256K or 2G-1)
// into the plain decimal the tunable commands accept.
func decimal(s string) (string, bool) {
	v, ok := aix.ParseTunableValue(s)
	if !ok {
		return "", false
	}
	return v.String(), true
}

// Device attributes.

func (r *run) deviceValue(device, attribute string) (aix.DeviceAttribute, error) {
	if r.dev.Runner == nil {
		return aix.DeviceAttribute{}, errNoRunner
	}
	a, err := r.dev.Get(r.ctx, device, attribute)
	if errors.Is(err, aix.ErrDeviceNotFound) {
		return a, reprovision("device %s does not exist on this node", device)
	}
	if err != nil {
		return a, err
	}
	if !a.UserSettable {
		return a, reprovision("%s attribute %s is not user-settable", device, attribute)
	}
	return a, nil
}

func (r *run) checkDevice(d rendered.DeviceAttribute) (status, string, error) {
	a, err := r.deviceValue(d.Device, d.Attribute)
	if err != nil {
		return inSync, "", err
	}
	if a.Value != d.Value {
		return needsChange, fmt.Sprintf("%s, want %s", a.Value, d.Value), nil
	}
	// After chdev -P the database already shows the new value; it is
	// effective only once the device is reconfigured at the next boot.
	if r.pendingThisBoot(d.Key(), d.Value) {
		return awaitingReboot, "", nil
	}
	return inSync, "", nil
}

func (r *run) baselineDevice(d rendered.DeviceAttribute) (Baseline, error) {
	a, err := r.dev.Get(r.ctx, d.Device, d.Attribute)
	if err != nil {
		return Baseline{}, err
	}
	return Baseline{Device: &ValueBaseline{Current: a.Value}}, nil
}

func (r *run) applyDevice(device, attribute, value, onBusy, key string) error {
	err := r.dev.Set(r.ctx, device, attribute, value)
	if err == nil {
		delete(r.st.PendingReboot, key)
		return nil
	}
	if !errors.Is(err, aix.ErrDeviceBusy) {
		return err
	}
	if onBusy == rendered.OnBusyFail {
		return fmt.Errorf("%w and onBusy is Fail", err)
	}
	if err := r.dev.SetDeferred(r.ctx, device, attribute, value); err != nil {
		return err
	}
	r.markPending(key, value)
	klog.Infof("%s is busy; %s=%s recorded for the next boot (chdev -P)", device, attribute, value)
	return nil
}

func (r *run) restoreDevice(d rendered.DeviceAttribute, value string) error {
	a, err := r.dev.Get(r.ctx, d.Device, d.Attribute)
	if errors.Is(err, aix.ErrDeviceNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if a.Value == value {
		delete(r.st.PendingReboot, d.Key())
		return nil
	}
	return r.applyDevice(d.Device, d.Attribute, value, rendered.OnBusyDefer, d.Key())
}
