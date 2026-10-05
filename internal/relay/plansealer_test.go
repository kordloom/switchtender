package relay_test

import "encoding/base64"

// planSealerStub seals a plan file by encoding it, standing in for the control node's key.
type planSealerStub struct{}

// SealPlanFile returns plan encoded under a marker, the shape a sealed value takes at rest.
func (planSealerStub) SealPlanFile(plan []byte) (string, error) {
	return "sealed:" + base64.StdEncoding.EncodeToString(plan), nil
}
