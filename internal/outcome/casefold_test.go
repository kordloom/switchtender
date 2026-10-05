package outcome

import "testing"

// TestParseRefusesCaseVariantKeys is the guarantee that the outcome summary reads the member a reader
// sees. A body carrying both status and Status would fold the variant onto the Status field under a
// case-insensitive struct decode, so the verify summary could call a run executed over a body whose
// exact status says otherwise. The body is refused instead.
func TestParseRefusesCaseVariantKeys(t *testing.T) {
	t.Parallel()
	// The negative control: a clean body parses and its status is read.
	rec, err := Parse([]byte(`{"run_id":"r1","status":"succeeded"}`))
	if err != nil {
		t.Fatalf("a clean outcome body did not parse: %v", err)
	}
	if rec.Status != "succeeded" {
		t.Fatalf("status = %q, want succeeded", rec.Status)
	}

	tests := []struct {
		Name string
		Body string
	}{
		{Name: "top-level status", Body: `{"status":"rejected","Status":"succeeded"}`},
		{Name: "nested object", Body: `{"status":"succeeded","initiator":{"agent":"a","Agent":"b"}}`},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse([]byte(test.Body)); err == nil {
				t.Errorf("a body with a case-variant member (%s) parsed rather than being refused",
					test.Name)
			}
		})
	}
}
