// Package version reports the AMCO build version.
package version

// Version is set at build time with
// -ldflags "-X github.com/gilbahat/aix-machine-config-operator/pkg/version.Version=...".
var Version = "0.0.0-dev"
