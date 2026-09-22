package outcome_test

import (
	"encoding/json"

	"github.com/kordloom/switchtender/internal/run"
)

// runJSON renders a run the way an API response does, so a test can assert what a caller is shown.
func runJSON(r *run.Run) (string, error) {
	body, err := json.Marshal(r)
	return string(body), err
}
