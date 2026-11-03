// Code in this file is copied from github.com/openshift/machine-config-operator
// pkg/controller/container-runtime-config/helpers.go at commit
// 0e2557038232d89cfbf810a6aabf85430d472211 (branch release-5.0),
// licensed under the Apache License, Version 2.0. The functions are unexported
// upstream, so they are copied here verbatim apart from being exported.
// Keep them in sync when the MCO pin moves (see hack/check-upstream-copies.sh).

package upstream

import (
	"fmt"
	"regexp"

	apicfgv1 "github.com/openshift/api/config/v1"
	apioperatorsv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	"github.com/openshift/runtime-utils/pkg/registries"
)

var (
	// sourceRegex and mirrorRegex pattern should stay the same with https://github.com/openshift/api/blob/ef62af078a9387e739abd99ec1d80e9129bb5475/config/v1/types_image_digest_mirror_set.go
	// Validation the source and mirror format for IDMS/ITMS already exists in the CRD. We need to keep this regex validation for ICSP
	sourceRegex = regexp.MustCompile(`^\*(?:\.(?:[a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9-]*[a-zA-Z0-9]))+$|^((?:[a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9-]*[a-zA-Z0-9])(?:(?:\.(?:[a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9-]*[a-zA-Z0-9]))+)?(?::[0-9]+)?)(?:(?:/[a-z0-9]+(?:(?:(?:[._]|__|[-]*)[a-z0-9]+)+)?)+)?$`)
	mirrorRegex = regexp.MustCompile(`^((?:[a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9-]*[a-zA-Z0-9])(?:(?:\.(?:[a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9-]*[a-zA-Z0-9]))+)?(?::[0-9]+)?)(?:(?:/[a-z0-9]+(?:(?:(?:[._]|__|[-]*)[a-z0-9]+)+)?)+)?$`)
)

// ValidateRegistriesConfScopes is upstream validateRegistriesConfScopes.
func ValidateRegistriesConfScopes(insecure, blocked, allowed []string, icspRules []*apioperatorsv1alpha1.ImageContentSourcePolicy, idmsRules []*apicfgv1.ImageDigestMirrorSet, itmsRules []*apicfgv1.ImageTagMirrorSet) error {
	for _, scope := range insecure {
		if !registries.IsValidRegistriesConfScope(scope) {
			return fmt.Errorf("invalid entry for insecure registries %q", scope)
		}
	}

	for _, scope := range blocked {
		if !registries.IsValidRegistriesConfScope(scope) {
			return fmt.Errorf("invalid entry for blocked registries %q", scope)
		}
	}

	for _, scope := range allowed {
		if !registries.IsValidRegistriesConfScope(scope) {
			return fmt.Errorf("invalid entry for allowed registries %q", scope)
		}
	}

	sourcePolicy := map[string]apicfgv1.MirrorSourcePolicy{}
	validateMirrors := func(source string, mirrors []apicfgv1.ImageMirror, policy apicfgv1.MirrorSourcePolicy) error {
		if !sourceRegex.MatchString(source) {
			return fmt.Errorf("invalid format for source %q", source)
		}
		if policy == "" {
			policy = apicfgv1.AllowContactingSource
		}
		if p, ok := sourcePolicy[source]; ok {
			if policy != p {
				return fmt.Errorf("conflicting mirrorSourcePolicy is set for the same source %q in imagedigestmirrorsets, imagetagmirrorsets, or imagecontentsourcepolicies", source)
			}
		} else {
			sourcePolicy[source] = policy
		}

		for _, mirror := range mirrors {
			if !mirrorRegex.MatchString(string(mirror)) {
				return fmt.Errorf("invalid format for mirror %q", mirror)
			}
			if policy == apicfgv1.NeverContactSource && string(mirror) == source {
				return fmt.Errorf("cannot set mirrorSourcePolicy: NeverContactSource if the source %q is one of the mirrors", source)
			}
		}
		return nil
	}

	for _, icsp := range icspRules {
		idmsRules = append(idmsRules, ConvertICSPToIDMS(icsp))
	}

	for _, idms := range idmsRules {
		for _, mirrorSet := range idms.Spec.ImageDigestMirrors {
			if err := validateMirrors(mirrorSet.Source, mirrorSet.Mirrors, mirrorSet.MirrorSourcePolicy); err != nil {
				return err
			}
		}
	}

	for _, itms := range itmsRules {
		for _, mirrorSet := range itms.Spec.ImageTagMirrors {
			if err := validateMirrors(mirrorSet.Source, mirrorSet.Mirrors, mirrorSet.MirrorSourcePolicy); err != nil {
				return err
			}
		}
	}
	return nil
}

// ConvertICSPToIDMS (upstream convertICSPToIDMS) converts ImageContentSourcePolicy to ImageDigestMirrorSet struct
func ConvertICSPToIDMS(icsp *apioperatorsv1alpha1.ImageContentSourcePolicy) *apicfgv1.ImageDigestMirrorSet {
	var imageDigestMirrors []apicfgv1.ImageDigestMirrors

	for _, mirrorSet := range icsp.Spec.RepositoryDigestMirrors {
		var mirrors []apicfgv1.ImageMirror
		for _, mirror := range mirrorSet.Mirrors {
			mirrors = append(mirrors, apicfgv1.ImageMirror(mirror))
		}
		imageDigestMirror := apicfgv1.ImageDigestMirrors{
			Source:  mirrorSet.Source,
			Mirrors: mirrors,
		}
		imageDigestMirrors = append(imageDigestMirrors, imageDigestMirror)
	}
	return &apicfgv1.ImageDigestMirrorSet{
		Spec: apicfgv1.ImageDigestMirrorSetSpec{
			ImageDigestMirrors: imageDigestMirrors,
		},
	}
}
