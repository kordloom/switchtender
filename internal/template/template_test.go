package template_test

import (
	"testing"

	"github.com/dcadolph/yardmaster/internal/template"
	"github.com/dcadolph/yardmaster/internal/templatetest"
)

func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	templatetest.Contract(t, func() template.Store { return template.NewMemStore() })
}
