package team_test

import (
	"testing"

	"github.com/dcadolph/switchtender/internal/team"
	"github.com/dcadolph/switchtender/internal/teamtest"
)

func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	teamtest.Contract(t, func() team.Store { return team.NewMemStore() })
}
