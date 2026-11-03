package render

import (
	"fmt"
	"path"
	"strings"

	"github.com/containers/image/v5/pkg/sysregistriesv2"
	configv1 "github.com/openshift/api/config/v1"
	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"
	"github.com/openshift/runtime-utils/pkg/registries"

	"github.com/gilbahat/aix-machine-config-operator/pkg/rendered"
	"github.com/gilbahat/aix-machine-config-operator/pkg/upstream"
)

// renderRegistries turns the cluster's registry configuration into
// containerd hosts.toml files, one per registry host.
//
// The MCO builds /etc/containers/registries.conf for CRI-O with
// runtime-utils' EditRegistriesConfig. AMCO calls the same function to get
// the same merged view of the Image config, IDMS and ITMS objects, then
// translates each registry entry to containerd's per-host format
// (https://github.com/containerd/containerd/blob/main/docs/hosts.md).
// containerd's format is less expressive than registries.conf; anything that
// cannot be translated is reported as a warning rather than failing the pool.
func renderRegistries(cc *mcfgv1.ControllerConfig, img *configv1.Image, idms []*configv1.ImageDigestMirrorSet, itms []*configv1.ImageTagMirrorSet) ([]rendered.File, []string, error) {
	var insecure, blocked, allowed, search []string
	if img != nil {
		insecure = img.Spec.RegistrySources.InsecureRegistries
		blocked = img.Spec.RegistrySources.BlockedRegistries
		allowed = img.Spec.RegistrySources.AllowedRegistries
		search = img.Spec.RegistrySources.ContainerRuntimeSearchRegistries
	}
	if err := upstream.ValidateRegistriesConfScopes(insecure, blocked, allowed, nil, idms, itms); err != nil {
		return nil, nil, err
	}
	conf := sysregistriesv2.V2RegistriesConf{}
	if err := registries.EditRegistriesConfig(&conf, insecure, blocked, nil, idms, itms); err != nil {
		return nil, nil, err
	}

	var warnings []string
	if len(allowed) > 0 {
		warnings = append(warnings, "image config allowedRegistries is not enforced on AIX nodes: containerd has no registry allow-list")
	}
	if len(search) > 0 {
		warnings = append(warnings, "image config containerRuntimeSearchRegistries is ignored on AIX nodes: containerd does not resolve unqualified image names")
	}

	cas, err := registryCAs(cc)
	if err != nil {
		return nil, nil, err
	}
	hosts := map[string]*hostConfig{}
	get := func(host string) *hostConfig {
		h, ok := hosts[host]
		if !ok {
			h = &hostConfig{host: host}
			hosts[host] = h
		}
		return h
	}

	for i := range conf.Registries {
		reg := &conf.Registries[i]
		scope := reg.Location
		if scope == "" {
			scope = reg.Prefix
		}
		if strings.HasPrefix(scope, "*.") {
			warnings = append(warnings, fmt.Sprintf("wildcard registry scope %q is not supported by containerd hosts.toml; ignored on AIX nodes", scope))
			continue
		}
		host, repo := splitScope(scope)
		if !registryHostRE.MatchString(host) {
			return nil, nil, fmt.Errorf("registry scope %q does not start with a valid host[:port]", scope)
		}
		h := get(host)
		if reg.Insecure {
			h.skipVerify = true
		}
		if reg.Blocked {
			switch {
			case len(reg.Mirrors) == 0:
				warnings = append(warnings, fmt.Sprintf("blocked registry %q is not enforced on AIX nodes: containerd has no registry block-list", scope))
			case repo != "":
				warnings = append(warnings, fmt.Sprintf("mirrorSourcePolicy NeverContactSource for %q cannot be enforced on AIX nodes: containerd mirrors are per host, so %s stays reachable as a fallback", scope, host))
			default:
				h.neverContactSource = true
			}
		}
		if repo != "" && len(reg.Mirrors) > 0 {
			warnings = append(warnings, fmt.Sprintf("mirrors of %q apply to every repository on %s on AIX nodes (containerd mirrors are per host); images a mirror does not have are pulled from %s", scope, host, host))
		}
		for _, m := range reg.Mirrors {
			if mh, _ := splitScope(m.Location); !registryHostRE.MatchString(mh) {
				return nil, nil, fmt.Errorf("mirror %q of %q does not start with a valid host[:port]", m.Location, scope)
			}
			e, err := mirrorEntry(repo, m, reg.MirrorByDigestOnly)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("mirror %q of %q: %v; ignored on AIX nodes", m.Location, scope, err))
				continue
			}
			mh, _ := splitScope(m.Location)
			if _, ok := cas[mh]; ok {
				e.ca = registryCAPath(mh)
			}
			h.addMirror(e)
		}
	}
	for host := range cas {
		get(host).ca = registryCAPath(host)
	}

	var files []rendered.File
	for _, h := range hosts {
		files = append(files, rendered.File{
			Path:     path.Join(CertsDir, h.host, "hosts.toml"),
			Mode:     0o644,
			Contents: h.toml(),
			Group:    rendered.GroupRegistries,
		})
	}
	return files, warnings, nil
}

// splitScope splits "host[:port][/repo/path]" into host and repository path.
func splitScope(scope string) (host, repo string) {
	host, repo, _ = strings.Cut(scope, "/")
	return host, repo
}

// mirrorEntry translates one registries.conf mirror of a source repository
// (repo is "" for a whole-registry source) into a hosts.toml host entry.
//
// containerd sends a mirror the request's own repository path; with
// override_path the mirror URL replaces the "/v2" API root, which lets a
// mirror add a path prefix but not rename the repository. So a mirror of
// quay.io/ns/repo at mirror.example.com/quay/ns/repo can be expressed (API
// root mirror.example.com/v2/quay), but one at mirror.example.com/other cannot.
func mirrorEntry(repo string, m sysregistriesv2.Endpoint, byDigestOnly bool) (hostEntry, error) {
	mhost, mpath := splitScope(m.Location)
	prefix := mpath
	if repo != "" {
		switch {
		case mpath == repo:
			prefix = ""
		case strings.HasSuffix(mpath, "/"+repo):
			prefix = strings.TrimSuffix(mpath, "/"+repo)
		default:
			return hostEntry{}, fmt.Errorf("containerd cannot map repository %q to mirror path %q (a mirror may only add a path prefix)", repo, mpath)
		}
	}
	e := hostEntry{url: scheme(m.Insecure) + mhost, skipVerify: m.Insecure, capabilities: []string{"pull", "resolve"}}
	if prefix != "" {
		e.url += "/v2/" + prefix
		e.overridePath = true
	}
	// Digest-only mirrors (IDMS) must not resolve tags; that is containerd's
	// "resolve" capability. Tag-only mirrors (ITMS) need both.
	if m.PullFromMirror == sysregistriesv2.MirrorByDigestOnly || (byDigestOnly && m.PullFromMirror == "") {
		e.capabilities = []string{"pull"}
	}
	return e, nil
}

type hostEntry struct {
	url          string
	capabilities []string
	ca           string
	skipVerify   bool
	overridePath bool
}

type hostConfig struct {
	host               string
	ca                 string
	skipVerify         bool
	neverContactSource bool
	mirrors            []hostEntry
}

// addMirror appends e, merging it with an existing entry for the same URL
// (two repository-scoped sources on one host can share a mirror).
func (h *hostConfig) addMirror(e hostEntry) {
	for i := range h.mirrors {
		x := &h.mirrors[i]
		if x.url != e.url {
			continue
		}
		caps := map[string]bool{}
		for _, c := range append(x.capabilities, e.capabilities...) {
			caps[c] = true
		}
		x.capabilities = nil
		for _, c := range []string{"pull", "resolve"} {
			if caps[c] {
				x.capabilities = append(x.capabilities, c)
			}
		}
		x.skipVerify = x.skipVerify || e.skipVerify
		if x.ca == "" {
			x.ca = e.ca
		}
		return
	}
	h.mirrors = append(h.mirrors, e)
}

// scheme returns the URL scheme for a host entry. An insecure registry in
// OpenShift may speak plain HTTP or unverified TLS; containerd tries HTTPS
// first and falls back to HTTP only for an "http://" host with TLS settings
// (skip_verify), which is how insecure entries are written.
func scheme(insecure bool) string {
	if insecure {
		return "http://"
	}
	return "https://"
}

// serverURL is the registry's own endpoint. Docker Hub's API lives on a
// different host from its image names, as in containerd's defaults.
func (h *hostConfig) serverURL() string {
	if h.host == "docker.io" {
		return scheme(h.skipVerify) + "registry-1.docker.io"
	}
	return scheme(h.skipVerify) + h.host
}

func (h *hostConfig) toml() string {
	var b strings.Builder
	b.WriteString("# Generated by the AIX Machine Config Operator. Do not edit.\n")
	mirrors := h.mirrors
	// The top-level "server" is tried after every [host] entry and gets all
	// capabilities unless told otherwise.
	server := hostEntry{url: h.serverURL(), ca: h.ca, skipVerify: h.skipVerify}
	if h.neverContactSource && len(mirrors) > 0 {
		// Make the last mirror the server so the source registry is never
		// part of the host list.
		server = mirrors[len(mirrors)-1]
		mirrors = mirrors[:len(mirrors)-1]
		fmt.Fprintf(&b, "# mirrorSourcePolicy NeverContactSource: %s itself is never contacted.\n", h.host)
	}
	fmt.Fprintf(&b, "server = %s\n", tomlString(server.url))
	writeHostFields(&b, "", server)
	for _, m := range mirrors {
		fmt.Fprintf(&b, "\n[host.%s]\n", tomlString(m.url))
		writeHostFields(&b, "  ", m)
	}
	return b.String()
}

func writeHostFields(b *strings.Builder, indent string, e hostEntry) {
	if e.capabilities != nil {
		caps := make([]string, len(e.capabilities))
		for i, c := range e.capabilities {
			caps[i] = tomlString(c)
		}
		fmt.Fprintf(b, "%scapabilities = [%s]\n", indent, strings.Join(caps, ", "))
	}
	if e.overridePath {
		fmt.Fprintf(b, "%soverride_path = true\n", indent)
	}
	if e.ca != "" {
		fmt.Fprintf(b, "%sca = %s\n", indent, tomlString(e.ca))
	}
	if e.skipVerify {
		fmt.Fprintf(b, "%sskip_verify = true\n", indent)
	}
}

// tomlString quotes s as a TOML basic string.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, "\\u%04X", r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
