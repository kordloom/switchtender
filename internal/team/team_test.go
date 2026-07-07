package team_test

import (
	"testing"

	"github.com/dcadolph/yardmaster/internal/team"
	"github.com/dcadolph/yardmaster/internal/teamtest"
)

func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	teamtest.Contract(t, func() team.Store { return team.NewMemStore() })
}
