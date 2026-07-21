package project

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestGalaxyEnv covers the private galaxy server environment: none configured yields nothing, a
// server yields the server list and URL, and a token adds the token entry.
func TestGalaxyEnv(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name   string
		Server string
		Token  string
		Want   []string
	}{
		{"none", "", "", nil},
		{"server only", "https://hub.internal/api/galaxy/", "", []string{
			"ANSIBLE_GALAXY_SERVER_LIST=switchtender",
			"ANSIBLE_GALAXY_SERVER_SWITCHTENDER_URL=https://hub.internal/api/galaxy/",
		}},
		{"server and token", "https://hub.internal/api/galaxy/", "sekret", []string{
			"ANSIBLE_GALAXY_SERVER_LIST=switchtender",
			"ANSIBLE_GALAXY_SERVER_SWITCHTENDER_URL=https://hub.internal/api/galaxy/",
			"ANSIBLE_GALAXY_SERVER_SWITCHTENDER_TOKEN=sekret",
		}},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			s := &Syncer{galaxyServer: test.Server, galaxyToken: test.Token}
			if diff := cmp.Diff(test.Want, s.galaxyEnv()); diff != "" {
				t.Errorf("galaxyEnv() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
