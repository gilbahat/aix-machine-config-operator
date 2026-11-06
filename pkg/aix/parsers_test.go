package aix

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Sample outputs. Formats that could not be confirmed against a live AIX
// host are listed under "Verify on hardware" in the README.
const (
	// ps -o etime= -p 1: init's elapsed time, [[dd-]hh:]mm:ss (366 days,
	// 1 hour 39 minutes before 2027-11-07 09:00 UTC).
	sampleEtime = "366-01:39:00\n"

	sampleLsitab = "amcotest:2:once:/usr/bin/startsrc -s amcotest >/dev/console 2>&1\n"

	// vmo -x / vmo -F -x: tunable,current,default,reboot,min,max,unit,type,{dtunable }
	sampleVmoX = `ame_cpus_per_pool,n/a,8,8,1,1K,processors,B,
lgpg_regions,0,0,0,0,8E-1,,D,lgpg_size
maxfree,1088,1088,1088,16,200K,4KB pages,D,minfree memory_frames
minfree,960,960,960,8,200K,4KB pages,D,maxfree memory_frames
`
	sampleVmoFX = sampleVmoX + `kernel_heap_psize,65536,0,65536,0,16M,bytes,B,
lru_file_repage,0,0,0,0,1,boolean,D,
`
	sampleNoX = `tcp_recvspace,262144,16384,262144,4096,8E-1,byte,C,
tcp_sendspace,262144,16384,262144,4096,8E-1,byte,C,
ipqmaxlen,100,100,100,100,2G-1,numeric,R,
`
	sampleLsattr = "1024:True\n"

	sampleLssrcS = `#subsysname:synonym:cmdargs:path:uid:auditid:standin:standout:standerr:action:multi:contact:svrkey:svrmtype:priority:signorm:sigforce:display:waittime:grpname:
sshd_adm::-D -f /etc/ssh/sshd_config_adm:/usr/sbin/sshd:0:0:/dev/console:/dev/console:/dev/console:-R:-Q:-S:0:0:20:15:9:-d:20:ssh:
`
)

func TestBootTime(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"   05:07\n":      5*time.Minute + 7*time.Second,
		"02:05:07":        2*time.Hour + 5*time.Minute + 7*time.Second,
		" 412-02:05:07\n": 412*24*time.Hour + 2*time.Hour + 5*time.Minute + 7*time.Second,
	} {
		got, err := ParseElapsed(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, bad := range []string{"", "12", "a:b", "1-2-3:4"} {
		_, err := ParseElapsed(bad)
		assert.Error(t, err, bad)
	}
	now := time.Date(2027, 11, 7, 9, 0, 0, 0, time.UTC)
	r := newFakeRunner().on(CmdPs+" -o etime= -p 1", sampleEtime, nil)
	boot, err := BootTime(context.Background(), r, now)
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, 11, 6, 7, 21, 0, 0, time.UTC), boot, "uptimes over a year are unambiguous")

	assert.True(t, SameBoot("2026-11-06T07:21:00Z", "2026-11-06T07:21:40Z"))
	assert.False(t, SameBoot("2026-11-06T07:21:00Z", "2026-11-06T10:21:00Z"))
}

func TestParseLsitab(t *testing.T) {
	e, err := ParseLsitab([]byte(sampleLsitab))
	require.NoError(t, err)
	assert.Equal(t, InittabEntry{Identifier: "amcotest", RunLevel: "2", Action: "once", Command: "/usr/bin/startsrc -s amcotest >/dev/console 2>&1"}, e)
	e, err = ParseLsitab([]byte("x:2:once:/bin/echo a:b\n"))
	require.NoError(t, err)
	assert.Equal(t, "/bin/echo a:b", e.Command)
	_, err = ParseLsitab([]byte("garbage\n"))
	assert.Error(t, err)
}

func TestInittab(t *testing.T) {
	ctx := context.Background()
	e := InittabEntry{Identifier: "amcotest", RunLevel: "2", Action: "once", Command: "/usr/bin/true"}

	r := newFakeRunner().on(CmdLsitab+" amcotest", "", &CommandError{Err: errExit}).on(CmdMkitab+" "+e.String(), "", nil)
	require.NoError(t, Inittab{r}.Set(ctx, e))
	assert.Equal(t, []string{CmdLsitab + " amcotest", CmdMkitab + " " + e.String()}, r.calls)

	r = newFakeRunner().on(CmdLsitab+" amcotest", sampleLsitab, nil).on(CmdChitab+" "+e.String(), "", nil)
	require.NoError(t, Inittab{r}.Set(ctx, e))
	assert.Equal(t, CmdChitab+" "+e.String(), r.calls[1])

	r = newFakeRunner().on(CmdLsitab+" amcotest", "", &CommandError{Err: errExit})
	require.NoError(t, Inittab{r}.Remove(ctx, "amcotest"))
	assert.Len(t, r.calls, 1)

	for _, bad := range []InittabEntry{
		{Identifier: "-x", RunLevel: "2", Action: "once", Command: "c"},
		{Identifier: "waytoolongidentifier", RunLevel: "2", Action: "once", Command: "c"},
		{Identifier: "a", RunLevel: "2:", Action: "once", Command: "c"},
		{Identifier: "a", RunLevel: "2", Action: "sometimes", Command: "c"},
		{Identifier: "a", RunLevel: "2", Action: "once", Command: "c\nd:2:once:evil"},
	} {
		assert.Error(t, ValidateInittabEntry(bad), bad)
	}
}

func TestParseTunableListing(t *testing.T) {
	rows, err := ParseTunableListing([]byte(sampleVmoX))
	require.NoError(t, err)
	assert.Equal(t, TunableInfo{Name: "maxfree", Current: "1088", Default: "1088", Boot: "1088", Min: "16", Max: "200K", Unit: "4KB pages", Type: TunableDynamic}, rows["maxfree"])
	assert.Equal(t, TunableBosboot, rows["ame_cpus_per_pool"].Type)
	_, err = ParseTunableListing([]byte("vmo: 1485-110 Invalid tunable name\n"))
	assert.Error(t, err)
}

func TestTunableValues(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		eq   bool
	}{
		{"262144", "262144", true},
		{"256K", "262144", true},
		{"2G-1", "2147483647", true},
		{"8E-1", "9223372036854775807", true},
		{"16", "17", false},
		{"n/a", "n/a", true},
		{"n/a", "0", false},
	} {
		assert.Equal(t, tc.eq, TunableValuesEqual(tc.a, tc.b), "%s vs %s", tc.a, tc.b)
	}
}

func TestTunablesGetAndRestricted(t *testing.T) {
	ctx := context.Background()
	r := newFakeRunner().on("/usr/sbin/vmo -x", sampleVmoX, nil).on("/usr/sbin/vmo -F -x", sampleVmoFX, nil)
	tun := &Tunables{Runner: r}
	row, err := tun.Get(ctx, "vmo", "minfree")
	require.NoError(t, err)
	assert.False(t, row.Restricted)
	row, err = tun.Get(ctx, "vmo", "lru_file_repage")
	require.NoError(t, err)
	assert.True(t, row.Restricted)
	_, err = tun.Get(ctx, "vmo", "nonesuch")
	assert.ErrorIs(t, err, ErrUnknownTunable)
	assert.Len(t, r.calls, 2, "listings are cached")

	r.on("/usr/sbin/vmo -p -o minfree=1024", "Setting minfree to 1024\nSetting minfree to 1024 in nextboot file\n", nil)
	require.NoError(t, tun.SetNow(ctx, "vmo", "minfree", "1024"))
	r.on("/usr/sbin/vmo -r -o kernel_heap_psize=65536", "", nil)
	require.NoError(t, tun.SetReboot(ctx, "vmo", "kernel_heap_psize", "65536"))
	assert.Equal(t, []string{"n\n"}, r.inputs)

	_, err = (&Tunables{Runner: r}).Get(ctx, "sh", "x")
	assert.Error(t, err)
	assert.Error(t, ValidateTunable("vmo", "minfree", "1; reboot"))
	assert.Error(t, ValidateTunable("vmo", "-o", "1"))
	assert.Error(t, ValidateTunable("chdev", "x", "1"))
}

func TestDevices(t *testing.T) {
	ctx := context.Background()
	a, err := ParseLsattrValue([]byte(sampleLsattr))
	require.NoError(t, err)
	assert.Equal(t, DeviceAttribute{Value: "1024", UserSettable: true}, a)
	a, err = ParseLsattrValue([]byte("fe80::1:False\n"))
	require.NoError(t, err)
	assert.Equal(t, DeviceAttribute{Value: "fe80::1"}, a)

	r := newFakeRunner().
		on(CmdLsdev+" -C -l fcs0", "fcs0 Available 00-00 8Gb PCI Express Dual Port FC Adapter (df1000f114108a03)\n", nil).
		on(CmdLsattr+" -El fcs0 -a num_cmd_elems -F value:user_settable", sampleLsattr, nil).
		on(CmdChdev+" -l fcs0 -a num_cmd_elems=2048", "Method error (/usr/lib/methods/chgefc):\n\t0514-062 Cannot perform the requested function because the\n\t\t specified device is busy.\n", &CommandError{Err: errExit}).
		on(CmdChdev+" -l fcs0 -a num_cmd_elems=2048 -P", "fcs0 changed\n", nil).
		on(CmdLsdev+" -C -l fcs9", "", nil)
	d := Devices{r}
	got, err := d.Get(ctx, "fcs0", "num_cmd_elems")
	require.NoError(t, err)
	assert.Equal(t, "1024", got.Value)
	assert.ErrorIs(t, d.Set(ctx, "fcs0", "num_cmd_elems", "2048"), ErrDeviceBusy)
	require.NoError(t, d.SetDeferred(ctx, "fcs0", "num_cmd_elems", "2048"))
	_, err = d.Get(ctx, "fcs9", "num_cmd_elems")
	assert.ErrorIs(t, err, ErrDeviceNotFound)
	assert.Error(t, ValidateDeviceAttribute("fcs0", "num_cmd_elems", "1 -P"))
	assert.Error(t, ValidateDeviceAttribute("-l", "a", "1"))
}

func TestParseLssrcDefinition(t *testing.T) {
	d, err := ParseLssrcDefinition([]byte(sampleLssrcS))
	require.NoError(t, err)
	want := SubsystemDefinition{
		Path: "/usr/sbin/sshd", Arguments: "-D -f /etc/ssh/sshd_config_adm", UID: 0,
		Stdin: "/dev/console", Stdout: "/dev/console", Stderr: "/dev/console",
		Respawn: true, Contact: ContactSignal, StopSignal: 15, ForceSignal: 9, WaitTime: 20, Group: "ssh",
	}
	assert.Equal(t, want, d)
	assert.Equal(t, want, SubsystemDefinition{Path: "/usr/sbin/sshd", Arguments: "-D -f /etc/ssh/sshd_config_adm", Respawn: true, Group: "ssh"}.WithDefaults())

	_, err = ParseLssrcDefinition([]byte("0513-085 The foo Subsystem is not on file.\n"))
	assert.ErrorIs(t, err, ErrSubsystemNotFound)
}

func TestSubsystemDefinitionCommands(t *testing.T) {
	ctx := context.Background()
	d := SubsystemDefinition{Path: "/opt/app/bin/appd", Arguments: "-f /etc/app.conf", Group: "apps"}
	r := newFakeRunner().
		on(CmdMkssys+" -s appd -p /opt/app/bin/appd -u 0 -a -f /etc/app.conf -i /dev/console -o /dev/console -e /dev/console -O -Q -S -n 15 -f 9 -w 20 -G apps", "", nil).
		on(CmdChssys+" -s appd -p /opt/app/bin/appd -u 0 -a -f /etc/app.conf -i /dev/console -o /dev/console -e /dev/console -O -Q -S -n 15 -f 9 -w 20 -G apps", "", nil).
		on(CmdRmssys+" -s appd", "", nil)
	s := NewSRC(r)
	require.NoError(t, s.Define(ctx, "appd", d))
	require.NoError(t, s.Redefine(ctx, "appd", d))
	require.NoError(t, s.Undefine(ctx, "appd"))
	assert.Error(t, s.Define(ctx, "appd", SubsystemDefinition{Path: "relative"}))
	assert.Error(t, s.Define(ctx, "appd", SubsystemDefinition{Path: "/bin/x", Arguments: "a:b"}))
	assert.Error(t, s.Define(ctx, "-bad", d))
}
