package aix

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Device message codes.
const msgDeviceBusy = "0514-062" // Cannot perform the requested function because the specified device is busy.

// Patterns for device attributes; the CRD schema uses the same patterns.
const (
	// DeviceNamePattern matches AIX logical device names (fcs0, hdisk12, ent0, sys0).
	DeviceNamePattern = `^[a-z][a-z0-9_]{0,15}$`
	// AttributeNamePattern matches ODM attribute names (num_cmd_elems, max_xfer_size).
	AttributeNamePattern = `^[a-z][a-z0-9_]{0,31}$`
	// AttributeValuePattern keeps values to one token chdev parses as a value.
	AttributeValuePattern = `^[A-Za-z0-9_.:/+@-]{1,128}$`
)

var (
	deviceNameRE     = regexp.MustCompile(DeviceNamePattern)
	attributeNameRE  = regexp.MustCompile(AttributeNamePattern)
	attributeValueRE = regexp.MustCompile(AttributeValuePattern)
)

// ValidateDeviceAttribute checks a device, attribute and value before they
// reach lsattr or chdev.
func ValidateDeviceAttribute(device, attribute, value string) error {
	if !deviceNameRE.MatchString(device) {
		return fmt.Errorf("invalid device name %q", device)
	}
	if !attributeNameRE.MatchString(attribute) {
		return fmt.Errorf("device %s: invalid attribute name %q", device, attribute)
	}
	if !attributeValueRE.MatchString(value) {
		return fmt.Errorf("device %s: invalid value %q for %s", device, value, attribute)
	}
	return nil
}

// Errors from device operations.
var (
	ErrDeviceNotFound    = errors.New("no such device")
	ErrAttributeNotFound = errors.New("the device has no such attribute")
	ErrDeviceBusy        = errors.New("the device is busy")
)

// DeviceAttribute is a value as lsattr reports it.
type DeviceAttribute struct {
	Value        string
	UserSettable bool
}

// ParseLsattrValue parses `lsattr -El <dev> -a <attr> -F value:user_settable`.
func ParseLsattrValue(out []byte) (DeviceAttribute, error) {
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0])
	i := strings.LastIndex(line, ":")
	if i < 0 {
		return DeviceAttribute{}, fmt.Errorf("unexpected lsattr output %q", truncateOutput(out))
	}
	settable := strings.TrimSpace(line[i+1:])
	switch settable {
	case "True", "False", "True+":
	default:
		return DeviceAttribute{}, fmt.Errorf("unexpected lsattr user_settable %q", settable)
	}
	return DeviceAttribute{Value: line[:i], UserSettable: strings.HasPrefix(settable, "True")}, nil
}

// Devices reads and changes device attributes.
type Devices struct{ Runner Runner }

// Exists reports whether device is in the customized device database
// (`lsdev -C -l <dev>` prints nothing for an unknown device).
func (d Devices) Exists(ctx context.Context, device string) (bool, error) {
	if !deviceNameRE.MatchString(device) {
		return false, fmt.Errorf("invalid device name %q", device)
	}
	out, err := d.Runner.Run(ctx, CmdLsdev, "-C", "-l", device)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// Get returns an attribute's value from the customized database.
func (d Devices) Get(ctx context.Context, device, attribute string) (DeviceAttribute, error) {
	if !deviceNameRE.MatchString(device) || !attributeNameRE.MatchString(attribute) {
		return DeviceAttribute{}, fmt.Errorf("invalid device %q or attribute %q", device, attribute)
	}
	ok, err := d.Exists(ctx, device)
	if err != nil {
		return DeviceAttribute{}, err
	}
	if !ok {
		return DeviceAttribute{}, fmt.Errorf("%s: %w", device, ErrDeviceNotFound)
	}
	out, err := d.Runner.Run(ctx, CmdLsattr, "-El", device, "-a", attribute, "-F", "value:user_settable")
	if err != nil {
		// lsattr exits 255 when the attribute does not exist.
		return DeviceAttribute{}, fmt.Errorf("%s %s: %w (%v)", device, attribute, ErrAttributeNotFound, err)
	}
	return ParseLsattrValue(out)
}

// Set changes an attribute on the running device (chdev). If the device is
// busy it returns ErrDeviceBusy without changing anything.
func (d Devices) Set(ctx context.Context, device, attribute, value string) error {
	if err := ValidateDeviceAttribute(device, attribute, value); err != nil {
		return err
	}
	out, err := d.Runner.Run(ctx, CmdChdev, "-l", device, "-a", attribute+"="+value)
	if err != nil && hasMessageCode(out, msgDeviceBusy) {
		return fmt.Errorf("%s: %w", device, ErrDeviceBusy)
	}
	return err
}

// SetDeferred changes an attribute in the database only (chdev -P); the
// device picks it up when it is next configured, normally at boot.
func (d Devices) SetDeferred(ctx context.Context, device, attribute, value string) error {
	if err := ValidateDeviceAttribute(device, attribute, value); err != nil {
		return err
	}
	_, err := d.Runner.Run(ctx, CmdChdev, "-l", device, "-a", attribute+"="+value, "-P")
	return err
}
