package v1alpha1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression (second review): a 64-bit value must not break decoding a list.
func TestItemValueDecoding(t *testing.T) {
	var l AIXMachineConfigList
	require.NoError(t, json.Unmarshal([]byte(`{"items":[{"metadata":{"name":"a"},"spec":{
		"tunables":[{"command":"vmo","name":"x","value":4294967296}],
		"deviceAttributes":[{"device":"fcs0","attribute":"max_xfer_size","value":"0x200000"}]}},{"metadata":{"name":"b"}}]}`), &l))
	require.Len(t, l.Items, 2)
	assert.Equal(t, ItemValue("4294967296"), l.Items[0].Spec.Tunables[0].Value)
	assert.Equal(t, ItemValue("0x200000"), l.Items[0].Spec.DeviceAttributes[0].Value)
	out, err := json.Marshal(l.Items[0].Spec.Tunables[0])
	require.NoError(t, err)
	assert.Contains(t, string(out), `"value":"4294967296"`)
}
