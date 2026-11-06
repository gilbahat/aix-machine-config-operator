package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/klog/v2"

	"github.com/gilbahat/aix-machine-config-operator/pkg/aix"
	"github.com/gilbahat/aix-machine-config-operator/pkg/nodestate"
	"github.com/gilbahat/aix-machine-config-operator/pkg/rendered"
)

// Engine converges a host to a rendered config in place, item by item, the
// way an Ansible module does: read the live state, change only what differs,
// verify. It never reboots or shuts down the host. Every item ends in one of
// these outcomes:
//
//   - Live: applied and verified.
//   - ServiceRestart: applied, then an SRC subsystem was restarted or
//     refreshed (the operator drains first only for items that ask for it).
//   - PendingReboot: applied persistently (nextboot tunables, chdev -P);
//     takes effect at the next boot, whoever performs it.
//   - ReprovisionRequired: cannot converge in place (a device or subsystem
//     the LPAR does not have, an attribute that is not user-settable). The
//     item is skipped and reported, unless it is marked required.
type Engine struct {
	Files Host
	// Runner runs AIX commands. Without one only files can be applied and
	// subsystem actions are skipped (offline dry runs on any OS).
	Runner aix.Runner
	// NoRestart skips subsystem restarts and refreshes.
	NoRestart bool
	// DryRun applies files only, under Files.Root, without ownership; every
	// other item is skipped. It is for trying configs off AIX.
	DryRun bool
	// BootTime is when the host booted (who -b); zero when unknown.
	BootTime time.Time

	// Hooks, for tests; nil uses the real implementation.
	Now       func() time.Time
	Chown     func(path string, uid, gid int) error
	FileOwner func(path string, fi os.FileInfo) (uid, gid int, ok bool)
	LookupUID func(name string) (int, error)
	LookupGID func(name string) (int, error)
}

// Result is what an apply left the host with.
type Result struct {
	// Changed lists the keys of items changed or restored.
	Changed []string
	// PendingReboot lists items that wait for the next boot.
	PendingReboot []string
	// Reprovision lists items that cannot converge in place.
	Reprovision []nodestate.ItemIssue
	// Bosboot is the last bosboot run since the boot, if any.
	Bosboot *nodestate.BosbootStatus
}

// reprovisionError marks an item that cannot converge in place.
type reprovisionError struct{ reason string }

func (e *reprovisionError) Error() string { return e.reason }

func reprovision(format string, a ...interface{}) error {
	return &reprovisionError{reason: fmt.Sprintf(format, a...)}
}

// status is an item's live state compared with its desired state.
type status int

const (
	inSync status = iota
	needsChange
	// awaitingReboot: set persistently, waiting for the next boot.
	awaitingReboot
)

// run holds one pass over the config.
type run struct {
	e    *Engine
	ctx  context.Context
	st   *State
	src  *aix.SRC
	tun  *aix.Tunables
	dev  aix.Devices
	itab aix.Inittab
	boot string

	// actions is st.OwedActions: subsystem actions owed by changes already
	// made, persisted so a crash or failure never loses one.
	actions map[string]rendered.SubsystemAction
	started map[string]bool
	// undefined holds subsystems this run removed (rmssys).
	undefined map[string]bool
	// desiredState is the state each managed subsystem should end in.
	desiredState map[string]string
	bosboot      bool
	changed      []string
}

// ApplyOptions modify an apply.
type ApplyOptions struct {
	// Force (the force file) re-runs every item's subsystem actions and
	// retries a bosboot that failed for this config.
	Force bool
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) bootKey() string {
	if e.BootTime.IsZero() {
		return ""
	}
	return e.BootTime.UTC().Format(time.RFC3339)
}

func (e *Engine) newRun(ctx context.Context, st *State) *run {
	if st.OwedActions == nil {
		st.OwedActions = map[string]rendered.SubsystemAction{}
	}
	r := &run{e: e, ctx: ctx, st: st, boot: e.bootKey(),
		actions: st.OwedActions, started: map[string]bool{}, undefined: map[string]bool{}, desiredState: map[string]string{}}
	if e.Runner != nil {
		r.src = aix.NewSRC(e.Runner)
		r.tun = &aix.Tunables{Runner: e.Runner}
		r.dev = aix.Devices{Runner: e.Runner}
		r.itab = aix.Inittab{Runner: e.Runner}
	}
	if st.Baselines == nil {
		st.Baselines = map[string]Baseline{}
	}
	if st.PendingReboot == nil {
		st.PendingReboot = map[string]PendingItem{}
	}
	return r
}

// ordered lists a config's items in apply order: files first (so services
// started or restarted later see them), then subsystems, inittab entries,
// tunables and device attributes.
func ordered(c *rendered.Config) []rendered.Item {
	if c == nil {
		return nil
	}
	c.Normalize()
	var out []rendered.Item
	for _, f := range c.Files {
		out = append(out, f)
	}
	for _, s := range c.Subsystems {
		out = append(out, s)
	}
	for _, e := range c.Inittab {
		out = append(out, e)
	}
	for _, t := range c.Tunables {
		out = append(out, t)
	}
	for _, d := range c.DeviceAttributes {
		out = append(out, d)
	}
	return out
}

func (r *run) save() error { return r.e.Files.SaveState(r.st) }

// Apply converges the host to cfg.
//
// Every change is preceded by a save of the state it depends on: the
// baseline, the subsystem actions it will owe, a pending-reboot entry. So an
// apply interrupted at any point can simply be run again: each item is
// re-checked against the live state, owed actions are still owed, and items
// of an earlier, unfinished config are still known and restored.
func (e *Engine) Apply(ctx context.Context, name string, cfg *rendered.Config, opts ApplyOptions) (*Result, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	st, err := e.Files.LoadState()
	if err != nil {
		return nil, err
	}
	if st.BosbootFailedFor == name && !opts.Force {
		msg := "bosboot failed"
		if st.Bosboot != nil && st.Bosboot.Error != "" {
			msg += ": " + st.Bosboot.Error
		}
		return nil, fmt.Errorf("%s while applying %s; it is not retried automatically (fix the cause, then create %s or roll out a new config)", msg, name, ForceFile)
	}
	r := e.newRun(ctx, st)
	if st.Pending != nil {
		klog.Info("the previous apply did not finish; resuming")
	}
	if opts.Force {
		st.RestoreFailures = nil
	}
	// Items of earlier unfinished configs that this config drops stay
	// known, so they are still restored.
	st.Leftover = withoutKeys(mergeItems(st.Leftover, st.Pending), cfg.Items())
	owned := st.Owned()
	for _, s := range cfg.Subsystems {
		if s.State != "" {
			r.desiredState[s.Name] = s.State
		}
	}

	// Check every item before changing anything, so a required item that
	// cannot converge blocks the whole config, not half of it. Record each
	// item's original state the first time it is seen.
	type step struct {
		item   rendered.Item
		status status
	}
	var steps []step
	var reprov []nodestate.ItemIssue
	skip := func(item rendered.Item, reason string) error {
		if rendered.PolicyOf(item).Required {
			return fmt.Errorf("%s cannot converge in place and is required: %s", item.Key(), reason)
		}
		klog.Warningf("%s cannot converge in place, skipping it: %s", item.Key(), reason)
		reprov = append(reprov, nodestate.ItemIssue{Item: item.Key(), Reason: reason})
		return nil
	}
	for _, item := range ordered(cfg) {
		if _, isFile := item.(rendered.File); e.DryRun && !isFile {
			klog.Infof("dry run: skipping %s", item.Key())
			continue
		}
		s, _, err := r.check(item)
		var rp *reprovisionError
		if errors.As(err, &rp) {
			if err := skip(item, rp.reason); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", item.Key(), err)
		}
		if _, ok := st.Baselines[item.Key()]; !ok {
			b, err := r.baseline(item)
			if err != nil {
				return nil, fmt.Errorf("%s: recording its original state: %w", item.Key(), err)
			}
			if err := validateBaseline(item, b); err != nil {
				if err := skip(item, fmt.Sprintf("its current state could not be restored later, so AMCO does not manage it: %v", err)); err != nil {
					return nil, err
				}
				continue
			}
			st.Baselines[item.Key()] = b
		}
		steps = append(steps, step{item, s})
	}
	st.Pending = cfg
	if err := r.save(); err != nil {
		return nil, fmt.Errorf("recording apply in progress: %w", err)
	}
	fail := func(err error) (*Result, error) {
		_ = r.save()
		return nil, err
	}

	for _, s := range steps {
		key := s.item.Key()
		if opts.Force {
			if err := r.queueActions(s.item); err != nil {
				return fail(err)
			}
		}
		switch s.status {
		case needsChange:
			if err := r.apply(s.item); err != nil {
				return fail(fmt.Errorf("%s: %w", key, err))
			}
			r.changed = append(r.changed, key)
		case inSync:
			delete(st.PendingReboot, key)
		}
	}

	// Items that left the config go back to their original state. A
	// restore that fails is reported and given up, never left to block
	// every later config.
	desired := cfg.Items()
	var removed []string
	for key := range owned {
		if _, ok := desired[key]; !ok {
			removed = append(removed, key)
		}
	}
	sort.Strings(removed)
	for _, key := range removed {
		if err := r.restore(key, owned[key]); err != nil {
			klog.Warningf("could not restore %s to its original state: %v", key, err)
			st.RestoreFailures = append(st.RestoreFailures, nodestate.ItemIssue{Item: key, Reason: "could not restore its original state: " + err.Error()})
		}
		r.changed = append(r.changed, key)
	}

	if err := r.runBosboot(name); err != nil {
		return nil, err
	}
	if err := r.runActions(); err != nil {
		return fail(err)
	}

	// Verify what changed actually converged.
	for _, s := range steps {
		if s.status != needsChange {
			continue
		}
		got, detail, err := r.check(s.item)
		if err != nil {
			return fail(fmt.Errorf("verifying %s: %w", s.item.Key(), err))
		}
		switch got {
		case needsChange:
			return fail(fmt.Errorf("%s did not converge: %s", s.item.Key(), detail))
		case inSync:
			delete(st.PendingReboot, s.item.Key())
		}
	}

	for _, key := range removed {
		delete(st.Baselines, key)
	}
	st.Name, st.Config, st.Pending, st.Leftover, st.Reprovision = name, cfg, nil, nil, reprov
	if err := r.save(); err != nil {
		return nil, err
	}
	sort.Strings(r.changed)
	klog.Infof("applied %s: %d item(s) changed, %d pending reboot, %d need reprovisioning", name, len(r.changed), len(st.PendingReboot), len(reprov))
	return &Result{Changed: r.changed, PendingReboot: st.PendingRebootKeys(), Reprovision: reprov, Bosboot: st.Bosboot}, nil
}

// mergeItems returns the union of two configs' items; b wins for a key.
func mergeItems(a, b *rendered.Config) *rendered.Config {
	if a == nil && b == nil {
		return nil
	}
	out := &rendered.Config{Version: rendered.FormatVersion}
	items := a.Items()
	for k, v := range b.Items() {
		items[k] = v
	}
	for _, it := range items {
		addItem(out, it)
	}
	return out
}

// withoutKeys returns c without the items whose keys are in drop.
func withoutKeys(c *rendered.Config, drop map[string]rendered.Item) *rendered.Config {
	if c == nil {
		return nil
	}
	out := &rendered.Config{Version: rendered.FormatVersion, Pool: c.Pool}
	for k, it := range c.Items() {
		if _, ok := drop[k]; !ok {
			addItem(out, it)
		}
	}
	if len(out.Items()) == 0 {
		return nil
	}
	out.Normalize()
	return out
}

func addItem(c *rendered.Config, it rendered.Item) {
	switch v := it.(type) {
	case rendered.File:
		c.Files = append(c.Files, v)
	case rendered.Subsystem:
		c.Subsystems = append(c.Subsystems, v)
	case rendered.InittabEntry:
		c.Inittab = append(c.Inittab, v)
	case rendered.Tunable:
		c.Tunables = append(c.Tunables, v)
	case rendered.DeviceAttribute:
		c.DeviceAttributes = append(c.DeviceAttributes, v)
	}
}

// validateBaseline checks that an item's original state is something AMCO
// could write back. If not, managing the item would make its removal fail
// forever, so the item is not managed at all.
func validateBaseline(item rendered.Item, b Baseline) error {
	switch it := item.(type) {
	case rendered.Subsystem:
		if b.Subsystem != nil && b.Subsystem.Definition != nil {
			d := *b.Subsystem.Definition
			if d.Contact != aix.ContactSignal && d.Contact != aix.ContactSocket {
				return fmt.Errorf("subsystem %s uses contact %q, which AMCO cannot recreate", it.Name, d.Contact)
			}
			return d.Validate()
		}
	case rendered.InittabEntry:
		if b.Inittab != nil && b.Inittab.Entry != nil {
			return aix.ValidateInittabEntry(*b.Inittab.Entry)
		}
	case rendered.Tunable:
		if b.Tunable != nil {
			if _, ok := decimal(b.Tunable.Boot); !ok {
				if _, ok := decimal(b.Tunable.Current); !ok {
					return fmt.Errorf("%s has no numeric value (%q)", it.Key(), b.Tunable.Current)
				}
			}
		}
	case rendered.DeviceAttribute:
		if b.Device != nil {
			return aix.ValidateDeviceAttribute(it.Device, it.Attribute, b.Device.Current)
		}
	}
	return nil
}

// Drift compares the live state of every applied item with the applied
// config. It only reads; drift is never repaired automatically.
func (e *Engine) Drift(ctx context.Context) ([]nodestate.ItemIssue, error) {
	st, err := e.Files.LoadState()
	if err != nil {
		return nil, err
	}
	if st.Config == nil {
		return nil, nil
	}
	skip := map[string]bool{}
	for _, i := range st.Reprovision {
		skip[i.Item] = true
	}
	r := e.newRun(ctx, st)
	out := append([]nodestate.ItemIssue(nil), st.RestoreFailures...)
	for _, item := range ordered(st.Config) {
		if _, isFile := item.(rendered.File); skip[item.Key()] || (e.DryRun && !isFile) {
			continue
		}
		s, detail, err := r.check(item)
		switch {
		case err != nil:
			out = append(out, nodestate.ItemIssue{Item: item.Key(), Reason: err.Error()})
		case s == needsChange:
			out = append(out, nodestate.ItemIssue{Item: item.Key(), Reason: detail})
		}
	}
	return out, nil
}

// ReconcileBoot clears pending-reboot items set during an earlier boot: the
// boot they waited for has happened, whoever performed it. Whether they are
// now effective is checked by the next drift check, which reports any that
// are not. It also forgets a bosboot that the current boot already used.
func (e *Engine) ReconcileBoot() (cleared []string, err error) {
	boot := e.bootKey()
	if boot == "" {
		return nil, nil
	}
	st, err := e.Files.LoadState()
	if err != nil {
		return nil, err
	}
	changed := false
	for key, p := range st.PendingReboot {
		if !aix.SameBoot(p.SetBoot, boot) {
			delete(st.PendingReboot, key)
			cleared = append(cleared, key)
			changed = true
		}
	}
	if st.Bosboot != nil && st.Bosboot.State == nodestate.BosbootRebuilt && !aix.SameBoot(st.BosbootBoot, boot) {
		st.Bosboot, st.BosbootBoot = nil, ""
		changed = true
	}
	if !changed {
		return nil, nil
	}
	sort.Strings(cleared)
	klog.Infof("the host rebooted; %d pending item(s) took their boot: %s", len(cleared), strings.Join(cleared, ", "))
	return cleared, e.Files.SaveState(st)
}

// check compares an item's live state with the desired state.
func (r *run) check(item rendered.Item) (status, string, error) {
	switch it := item.(type) {
	case rendered.File:
		return r.checkFile(it)
	case rendered.Subsystem:
		return r.checkSubsystem(it)
	case rendered.InittabEntry:
		return r.checkInittab(it)
	case rendered.Tunable:
		return r.checkTunable(it)
	case rendered.DeviceAttribute:
		return r.checkDevice(it)
	}
	return inSync, "", fmt.Errorf("unknown item type %T", item)
}

func (r *run) baseline(item rendered.Item) (Baseline, error) {
	switch it := item.(type) {
	case rendered.File:
		return r.baselineFile(it)
	case rendered.Subsystem:
		return r.baselineSubsystem(it)
	case rendered.InittabEntry:
		return r.baselineInittab(it)
	case rendered.Tunable:
		return r.baselineTunable(it)
	case rendered.DeviceAttribute:
		return r.baselineDevice(it)
	}
	return Baseline{}, fmt.Errorf("unknown item type %T", item)
}

func (r *run) apply(item rendered.Item) error {
	switch it := item.(type) {
	case rendered.File:
		return r.applyFile(it)
	case rendered.Subsystem:
		return r.applySubsystem(it)
	case rendered.InittabEntry:
		return r.itab.Set(r.ctx, it.InittabEntry)
	case rendered.Tunable:
		return r.applyTunable(it.Command, it.Name, it.Value, it.Key())
	case rendered.DeviceAttribute:
		return r.applyDevice(it.Device, it.Attribute, it.Value, it.OnBusy, it.Key())
	}
	return fmt.Errorf("unknown item type %T", item)
}

// restore puts an item that left the config back to its baseline.
func (r *run) restore(key string, item rendered.Item) error {
	b, ok := r.st.Baselines[key]
	if f, isFile := item.(rendered.File); isFile {
		return r.restoreFile(f, b.File, ok && b.File != nil)
	}
	if !ok {
		klog.Infof("%s left the config; it was never changed, so there is nothing to restore", key)
		delete(r.st.PendingReboot, key)
		return nil
	}
	switch it := item.(type) {
	case rendered.Subsystem:
		return r.restoreSubsystem(it.Name, b.Subsystem)
	case rendered.InittabEntry:
		if r.itab.Runner == nil {
			return errNoRunner
		}
		if b.Inittab == nil || b.Inittab.Entry == nil {
			return r.itab.Remove(r.ctx, it.Identifier)
		}
		return r.itab.Set(r.ctx, *b.Inittab.Entry)
	case rendered.Tunable:
		return r.restoreTunable(it, b.Tunable)
	case rendered.DeviceAttribute:
		if b.Device == nil {
			return nil
		}
		return r.restoreDevice(it, b.Device.Current)
	}
	return fmt.Errorf("unknown item type %T", item)
}

var errNoRunner = errors.New("this item needs AIX commands, which are not available (not running on AIX)")

// queueActions records, and saves, an item's subsystem actions; callers
// queue them before making the change that owes them. Restart wins over
// refresh for the same subsystem.
func (r *run) queueActions(item rendered.Item) error {
	f, ok := item.(rendered.File)
	if !ok || len(f.Actions) == 0 {
		return nil
	}
	for _, a := range f.Actions {
		r.owe(a)
	}
	return r.save()
}

func (r *run) owe(a rendered.SubsystemAction) {
	if r.undefined[a.Subsystem] {
		return
	}
	if prev, ok := r.actions[a.Subsystem]; ok && prev.Action == rendered.ActionRestart {
		return
	}
	r.actions[a.Subsystem] = a
}

// runActions restarts or refreshes the subsystems whose files changed:
// containerd before the kubelet, then the rest by name. Subsystems that are
// not running, that a config wants inoperative, or that were just started
// are left alone.
func (r *run) runActions() error {
	// A restart that stopped a subsystem and then failed is finished on
	// every run, whatever else is queued.
	for _, n := range r.st.Restarting {
		if _, ok := r.actions[n]; !ok {
			r.actions[n] = rendered.SubsystemAction{Subsystem: n, Action: rendered.ActionRestart, IgnoreMissing: true}
		}
	}
	if len(r.actions) == 0 {
		return nil
	}
	var names []string
	for n := range r.actions {
		names = append(names, n)
	}
	rank := func(n string) int {
		switch n {
		case rendered.SubsystemContainerd:
			return 0
		case rendered.SubsystemKubelet:
			return 1
		}
		return 2
	}
	sort.Slice(names, func(i, j int) bool {
		if rank(names[i]) != rank(names[j]) {
			return rank(names[i]) < rank(names[j])
		}
		return names[i] < names[j]
	})
	done := func(n string) error {
		delete(r.actions, n)
		r.st.Restarting = remove(r.st.Restarting, n)
		return r.save()
	}
	for _, n := range names {
		a := r.actions[n]
		switch {
		case r.e.NoRestart || r.src == nil:
			klog.Infof("not running %s of %s (restarts disabled)", a.Action, n)
			if err := done(n); err != nil {
				return err
			}
			continue
		case r.started[n], r.undefined[n], r.desiredState[n] == rendered.SubsystemInoperative:
			if err := done(n); err != nil {
				return err
			}
			continue
		}
		st, err := r.src.Status(r.ctx, n)
		if errors.Is(err, aix.ErrSubsystemNotFound) {
			if a.IgnoreMissing {
				klog.Warningf("%s is not defined on this host; skipping its %s", n, a.Action)
				if err := done(n); err != nil {
					return err
				}
				continue
			}
			return fmt.Errorf("%s of %s: %w", a.Action, n, err)
		}
		if err != nil {
			return err
		}
		resuming := contains(r.st.Restarting, n)
		if st.State != aix.StateActive && !resuming {
			klog.Infof("%s is %s; not running its %s", n, st.State, a.Action)
			if err := done(n); err != nil {
				return err
			}
			continue
		}
		klog.Infof("running %s of %s", a.Action, n)
		if a.Action == rendered.ActionRestart || resuming {
			if !resuming {
				r.st.Restarting = append(r.st.Restarting, n)
				if err := r.save(); err != nil {
					return err
				}
			}
			err = r.src.Restart(r.ctx, n)
		} else {
			err = r.src.Reload(r.ctx, n)
		}
		if err != nil {
			return fmt.Errorf("%s of %s: %w", a.Action, n, err)
		}
		if err := done(n); err != nil {
			return err
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func remove(list []string, s string) []string {
	var out []string
	for _, x := range list {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

// markPending records, and saves, that key waits for the next boot with
// value. It is called before the command that makes the change, so a crash
// in between never hides a change that is not yet effective.
func (r *run) markPending(key, value string) error {
	r.st.PendingReboot[key] = PendingItem{Value: value, SetBoot: r.boot}
	return r.save()
}

// pendingThisBoot reports whether key was set persistently during this boot.
func (r *run) pendingThisBoot(key, value string) bool {
	p, ok := r.st.PendingReboot[key]
	return ok && aix.SameBoot(p.SetBoot, r.boot) && p.Value == value
}

// runBosboot rebuilds the boot image when a Bosboot tunable changed. A
// failure is recorded and not retried automatically.
func (r *run) runBosboot(name string) error {
	if !r.bosboot && !r.st.BosbootNeeded {
		return nil
	}
	if r.e.Runner == nil {
		return errNoRunner
	}
	r.st.BosbootNeeded = true
	if err := r.save(); err != nil {
		return err
	}
	klog.Info("rebuilding the boot image (bosboot) for Bosboot tunables")
	now := r.e.now().UTC().Format(time.RFC3339)
	if err := aix.Bosboot(r.ctx, r.e.Runner); err != nil {
		r.st.Bosboot = &nodestate.BosbootStatus{State: nodestate.BosbootFailed, Time: now, Error: err.Error()}
		r.st.BosbootFailedFor, r.st.BosbootBoot = name, r.boot
		_ = r.save()
		return fmt.Errorf("bosboot failed (not retried automatically): %w", err)
	}
	r.st.Bosboot = &nodestate.BosbootStatus{State: nodestate.BosbootRebuilt, Time: now}
	r.st.BosbootNeeded, r.st.BosbootFailedFor, r.st.BosbootBoot = false, "", r.boot
	return r.save()
}

// Accounts.

func (r *run) uid(name string) (int, error) {
	if name == "" {
		return 0, nil
	}
	if n, err := strconv.Atoi(name); err == nil {
		return n, nil
	}
	if r.e.LookupUID != nil {
		return r.e.LookupUID(name)
	}
	u, err := user.Lookup(name)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(u.Uid)
}

func (r *run) gid(name string) (int, error) {
	if name == "" {
		return 0, nil
	}
	if n, err := strconv.Atoi(name); err == nil {
		return n, nil
	}
	if r.e.LookupGID != nil {
		return r.e.LookupGID(name)
	}
	g, err := user.LookupGroup(name)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(g.Gid)
}

func (r *run) owner(p string, fi os.FileInfo) (int, int, bool) {
	if r.e.DryRun {
		return 0, 0, false
	}
	if r.e.FileOwner != nil {
		return r.e.FileOwner(p, fi)
	}
	return fileOwner(fi)
}

func (r *run) chown(p string, uid, gid int) error {
	if r.e.DryRun {
		return nil
	}
	if r.e.Chown != nil {
		return r.e.Chown(p, uid, gid)
	}
	return os.Chown(p, uid, gid)
}
