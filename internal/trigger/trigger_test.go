package trigger_test

import (
	"strings"
	"testing"

	"github.com/dcadolph/yardmaster/internal/trigger"
	"github.com/dcadolph/yardmaster/internal/triggertest"
)

func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	triggertest.Contract(t, func() trigger.Store { return trigger.NewMemStore() })
}

func TestNew(t *testing.T) {
	t.Parallel()
	plain, tg, err := trigger.New("deploy", "tpl_1")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if !strings.HasPrefix(plain, "whk_") {
		t.Errorf("token = %q, want whk_ prefix", plain)
	}
	if tg.TokenHash != trigger.HashToken(plain) || tg.TemplateID != "tpl_1" {
		t.Errorf("trigger = %+v, want hash of plaintext and template tpl_1", tg)
	}
}
