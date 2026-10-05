package relay

import "encoding/base64"

// testPlanFile is the plan file the relay tests hand the control node.
var testPlanFile = []byte("relay test plan file")

// testPlanSealer seals a plan file by encoding it, standing in for the control node's key.
type testPlanSealer struct{}

// SealPlanFile returns plan encoded under a marker, the shape a sealed value takes at rest.
func (testPlanSealer) SealPlanFile(plan []byte) (string, error) {
	return "sealed:" + base64.StdEncoding.EncodeToString(plan), nil
}
