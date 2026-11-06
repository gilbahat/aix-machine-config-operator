package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gilbahat/aix-machine-config-operator/pkg/aix"
)

// fakeHost simulates the AIX commands the agent runs: SRC, inittab,
// tunables, devices, bosboot and who. It keeps the state those commands
// manage, records every command line, and can simulate a reboot.
type fakeHost struct {
	boot       time.Time
	subsystems map[string]*fakeSubsystem
	inittab    map[string]aix.InittabEntry
	tunables   map[string]map[string]*fakeTunable // command -> name
	devices    map[string]map[string]*fakeAttr    // device -> attribute
	busy       map[string]bool
	bosbootErr error
	startErr   map[string]error
	owners     map[string][2]int

	calls    []string
	mutating []string
	nextPID  int
}

type fakeSubsystem struct {
	def   aix.SubsystemDefinition
	state string
	pid   int
}

type fakeTunable struct {
	cur, boot  string
	typ        aix.TunableType
	restricted bool
}

type fakeAttr struct {
	odm, running string
	settable     bool
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		boot:       time.Date(2026, 11, 6, 7, 21, 0, 0, time.UTC),
		subsystems: map[string]*fakeSubsystem{},
		inittab:    map[string]aix.InittabEntry{},
		tunables:   map[string]map[string]*fakeTunable{},
		devices:    map[string]map[string]*fakeAttr{},
		busy:       map[string]bool{},
		startErr:   map[string]error{},
		owners:     map[string][2]int{},
		nextPID:    1000,
	}
}

func (h *fakeHost) defineSubsystem(name, state string) {
	h.subsystems[name] = &fakeSubsystem{def: aix.SubsystemDefinition{Path: "/usr/sbin/" + name}.WithDefaults(), state: state, pid: 1}
}

func (h *fakeHost) tunable(cmd, name, value string, typ aix.TunableType) {
	if h.tunables[cmd] == nil {
		h.tunables[cmd] = map[string]*fakeTunable{}
	}
	h.tunables[cmd][name] = &fakeTunable{cur: value, boot: value, typ: typ}
}

func (h *fakeHost) device(dev, attr, value string) {
	if h.devices[dev] == nil {
		h.devices[dev] = map[string]*fakeAttr{}
	}
	h.devices[dev][attr] = &fakeAttr{odm: value, running: value, settable: true}
}

// reboot simulates a reboot performed by someone else: next-boot tunables
// and deferred device attributes take effect.
func (h *fakeHost) reboot() {
	h.boot = h.boot.Add(3 * time.Hour)
	for _, ts := range h.tunables {
		for _, t := range ts {
			t.cur = t.boot
		}
	}
	for _, as := range h.devices {
		for _, a := range as {
			a.running = a.odm
		}
	}
	h.busy = map[string]bool{}
}

func (h *fakeHost) resetCalls() { h.calls, h.mutating = nil, nil }

var errExit = errors.New("exit status 1")

func fail(name string, args []string, out string) ([]byte, error) {
	return []byte(out), &aix.CommandError{Name: name, Args: args, Output: []byte(out), Err: errExit}
}

func (h *fakeHost) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return h.RunWithInput(ctx, nil, name, args...)
}

func (h *fakeHost) RunWithInput(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	h.calls = append(h.calls, line)
	mut := func() { h.mutating = append(h.mutating, line) }
	arg := func(i int) string {
		if i < len(args) {
			return args[i]
		}
		return ""
	}
	notOnFile := func(n string) ([]byte, error) {
		return fail(name, args, fmt.Sprintf("0513-085 The %s Subsystem is not on file.\n", n))
	}
	for cmd, path := range aix.TunableCommands {
		if name == path {
			return h.tunableCmd(cmd, name, args, mut)
		}
	}
	switch name {
	case aix.CmdWho:
		return []byte("   .        system boot  " + h.boot.Format("Jan 02 15:04") + "\n"), nil
	case aix.CmdLssrc:
		if arg(0) == "-S" {
			s, ok := h.subsystems[arg(2)]
			if !ok {
				return notOnFile(arg(2))
			}
			d := s.def
			action, multi, contact := "-O", "-Q", "-S"
			if d.Respawn {
				action = "-R"
			}
			if d.Multiple {
				multi = "-q"
			}
			if d.Contact == aix.ContactSocket {
				contact = "-K"
			}
			return []byte(fmt.Sprintf("#subsysname:synonym:cmdargs:path:uid:auditid:standin:standout:standerr:action:multi:contact:svrkey:svrmtype:priority:signorm:sigforce:display:waittime:grpname:\n%s::%s:%s:%d:0:%s:%s:%s:%s:%s:%s:0:0:20:%d:%d:-d:%d:%s:\n",
				arg(2), d.Arguments, d.Path, d.UID, d.Stdin, d.Stdout, d.Stderr, action, multi, contact, d.StopSignal, d.ForceSignal, d.WaitTime, d.Group)), nil
		}
		s, ok := h.subsystems[arg(1)]
		if !ok {
			return notOnFile(arg(1))
		}
		pid := ""
		if s.state == aix.StateActive {
			pid = strconv.Itoa(s.pid)
		}
		return []byte(fmt.Sprintf("Subsystem         Group            PID          Status \n %s %s %s %s\n", arg(1), s.def.Group, pid, s.state)), nil
	case aix.CmdStartsrc:
		mut()
		s, ok := h.subsystems[arg(1)]
		if !ok {
			return notOnFile(arg(1))
		}
		if err := h.startErr[arg(1)]; err != nil {
			return fail(name, args, err.Error())
		}
		h.nextPID++
		s.state, s.pid = aix.StateActive, h.nextPID
		return nil, nil
	case aix.CmdStopsrc:
		mut()
		n := args[len(args)-1]
		s, ok := h.subsystems[n]
		if !ok {
			return notOnFile(n)
		}
		s.state = aix.StateInoperative
		return nil, nil
	case aix.CmdRefresh, aix.CmdKill:
		mut()
		return nil, nil
	case aix.CmdMkssys, aix.CmdChssys:
		mut()
		n := arg(1)
		d, err := parseSSysFlags(args[2:])
		if err != nil {
			return fail(name, args, err.Error())
		}
		s, ok := h.subsystems[n]
		if name == aix.CmdMkssys {
			if ok {
				return fail(name, args, "0513-077 Subsystem has been defined already")
			}
			h.subsystems[n] = &fakeSubsystem{def: d, state: aix.StateInoperative}
			return nil, nil
		}
		if !ok {
			return notOnFile(n)
		}
		s.def = d
		return nil, nil
	case aix.CmdRmssys:
		mut()
		delete(h.subsystems, arg(1))
		return nil, nil
	case aix.CmdLsitab:
		e, ok := h.inittab[arg(0)]
		if !ok {
			return fail(name, args, "")
		}
		return []byte(e.String() + "\n"), nil
	case aix.CmdMkitab, aix.CmdChitab:
		mut()
		e, err := aix.ParseLsitab([]byte(arg(0)))
		if err != nil {
			return fail(name, args, err.Error())
		}
		h.inittab[e.Identifier] = e
		return nil, nil
	case aix.CmdRmitab:
		mut()
		delete(h.inittab, arg(0))
		return nil, nil
	case aix.CmdLsdev:
		if _, ok := h.devices[arg(2)]; !ok {
			return nil, nil
		}
		return []byte(arg(2) + " Available 00-00 Fake device\n"), nil
	case aix.CmdLsattr:
		a, ok := h.devices[arg(1)][arg(3)]
		if !ok {
			return fail(name, args, "lsattr: 0514-528 The attribute does not exist.\n")
		}
		settable := "False"
		if a.settable {
			settable = "True"
		}
		return []byte(a.odm + ":" + settable + "\n"), nil
	case aix.CmdChdev:
		mut()
		dev := arg(1)
		attr, value, _ := strings.Cut(arg(3), "=")
		a, ok := h.devices[dev][attr]
		if !ok {
			return fail(name, args, "0514-528 no such attribute")
		}
		if arg(4) == "-P" {
			a.odm = value
			return []byte(dev + " changed\n"), nil
		}
		if h.busy[dev] {
			return fail(name, args, "Method error (/usr/lib/methods/chgefc):\n\t0514-062 Cannot perform the requested function because the\n\t\t specified device is busy.\n")
		}
		a.odm, a.running = value, value
		return []byte(dev + " changed\n"), nil
	case aix.CmdBosboot:
		mut()
		if h.bosbootErr != nil {
			return fail(name, args, h.bosbootErr.Error())
		}
		return []byte("bosboot: Boot image is 51228 512 byte blocks.\n"), nil
	}
	return fail(name, args, "fakeHost: unexpected command "+line)
}

func (h *fakeHost) tunableCmd(cmd, name string, args []string, mut func()) ([]byte, error) {
	ts := h.tunables[cmd]
	switch strings.Join(args, " ") {
	case "-x", "-F -x":
		var names []string
		for n, t := range ts {
			if !t.restricted || len(args) == 2 {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		var b strings.Builder
		b.WriteString("dummy_tunable,0,0,0,0,1,boolean,D,\n")
		for _, n := range names {
			t := ts[n]
			fmt.Fprintf(&b, "%s,%s,0,%s,0,8E-1,numeric,%s,\n", n, t.cur, t.boot, t.typ)
		}
		return []byte(b.String()), nil
	}
	if len(args) == 3 && args[1] == "-o" {
		mut()
		n, v, _ := strings.Cut(args[2], "=")
		t, ok := ts[n]
		if !ok {
			return fail(name, args, "1485-110 Invalid tunable name "+n)
		}
		switch args[0] {
		case "-p":
			if t.typ == aix.TunableReboot || t.typ == aix.TunableBosboot || t.typ == aix.TunableStatic {
				return fail(name, args, "cannot change the current value of "+n)
			}
			t.cur, t.boot = v, v
		case "-r":
			t.boot = v
		}
		return nil, nil
	}
	return fail(name, args, "fakeHost: unexpected tunable command")
}

func parseSSysFlags(args []string) (aix.SubsystemDefinition, error) {
	var d aix.SubsystemDefinition
	for i := 0; i < len(args); i++ {
		next := func() string {
			i++
			if i < len(args) {
				return args[i]
			}
			return ""
		}
		switch args[i] {
		case "-p":
			d.Path = next()
		case "-a":
			d.Arguments = next()
		case "-u":
			d.UID, _ = strconv.Atoi(next())
		case "-i":
			d.Stdin = next()
		case "-o":
			d.Stdout = next()
		case "-e":
			d.Stderr = next()
		case "-R":
			d.Respawn = true
		case "-O":
			d.Respawn = false
		case "-q":
			d.Multiple = true
		case "-Q":
			d.Multiple = false
		case "-S":
			d.Contact = aix.ContactSignal
		case "-K":
			d.Contact = aix.ContactSocket
		case "-n":
			d.StopSignal, _ = strconv.Atoi(next())
		case "-f":
			d.ForceSignal, _ = strconv.Atoi(next())
		case "-w":
			d.WaitTime, _ = strconv.Atoi(next())
		case "-G":
			d.Group = next()
		default:
			return d, fmt.Errorf("unknown flag %s", args[i])
		}
	}
	return d, nil
}

// fileOwnerHook and chownHook let the engine see ownership as the fake
// records it, so tests do not depend on running as root.
func (h *fakeHost) chown(p string, uid, gid int) error {
	h.owners[p] = [2]int{uid, gid}
	return nil
}

func (h *fakeHost) fileOwner(p string, _ os.FileInfo) (int, int, bool) {
	o := h.owners[p]
	return o[0], o[1], true
}
