package v1alpha1

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/gilbahat/aix-machine-config-operator/pkg/aix"
	"github.com/gilbahat/aix-machine-config-operator/pkg/rendered"
)

// TestCRDPatternsMatchAgentValidation keeps the CRD schema's name patterns
// in step with the patterns the operator and the agent enforce.
func TestCRDPatternsMatchAgentValidation(t *testing.T) {
	data, err := os.ReadFile("../../../../manifests/07-machineconfig-crd.yaml")
	require.NoError(t, err)
	text := string(data)
	for name, pattern := range map[string]string{
		"SubsystemNamePattern":   aix.SubsystemNamePattern,
		"InittabIDPattern":       aix.InittabIDPattern,
		"InittabRunLevelPattern": aix.InittabRunLevelPattern,
		"TunableNamePattern":     aix.TunableNamePattern,
		"DeviceNamePattern":      aix.DeviceNamePattern,
		"AttributeNamePattern":   aix.AttributeNamePattern,
		"AccountPattern":         rendered.AccountPattern,
	} {
		assert.True(t, strings.Contains(text, "'"+pattern+"'"), "%s %s is not in the CRD", name, pattern)
	}
	for _, a := range aix.InittabActions {
		assert.Contains(t, text, a)
	}
	for cmd := range aix.TunableCommands {
		assert.Contains(t, text, cmd)
	}
	var crd map[string]interface{}
	require.NoError(t, yaml.Unmarshal(data, &crd))
}
