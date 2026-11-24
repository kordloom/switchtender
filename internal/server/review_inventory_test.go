package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestReviewHookRefusesAnInventoryThatRunsAProgram proves a pull request whose head carries an
// inventory that points the connection at a program on the controller is not planned: the playbook
// is clean, so only the inventory pass can see it. No run is created, and the pull request is told
// which setting stopped it and that removing it is the fix. A head whose inventory sets only the
// user, address, and port is planned.
func TestReviewHookRefusesAnInventoryThatRunsAProgram(t *testing.T) {
	t.Parallel()
	const clean = "- hosts: all\n  tasks:\n    - name: Show uptime\n" +
		"      ansible.builtin.command: uptime\n"
	tests := []struct {
		// Inventory is the hosts.ini the pull request's head carries.
		Inventory string
		// WantRefused is whether the plan is refused.
		WantRefused bool
	}{{ // Test 0: Bastion arguments that run a command.
		Inventory: "[all:vars]\nansible_ssh_common_args='-o ProxyCommand=\"sh -c id\"'\n\n" +
			"[web]\nweb1\n",
		WantRefused: true,
	}, { // Test 1: A clean inventory.
		Inventory: "[web]\nweb1 ansible_host=10.0.0.1 ansible_user=deploy ansible_port=2222\n",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			rs := newReviewServer(t, reviewSetup{Playbook: "site.yml",
				PRFiles: map[string]string{"site.yml": clean, "hosts.ini": test.Inventory}})
			rec := rs.fire(t, rs.eventName(), rs.payload(rs.opened(), rs.headSHA, ""), "")
			if rec.Code != http.StatusAccepted {
				t.Fatalf("webhook = %d %s, want 202", rec.Code, rec.Body.String())
			}
			if !test.WantRefused {
				if r := rs.planRun(t, rec); !r.DryRun {
					t.Error("the plan is not a dry run")
				}
				return
			}
			rs.waitStatus(t, rs.headSHA, "error")
			rs.srv.reviews.Wait()
			list, err := rs.runs.List(ctx)
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if len(list) != 0 {
				t.Errorf("a refused plan created %d runs", len(list))
			}
			c := rs.forge.Comments(7)
			if len(c) != 1 || !strings.Contains(c[0].Body, "ansible_ssh_common_args") ||
				!strings.Contains(c[0].Body, "Remove each ansible_ setting") ||
				strings.Contains(c[0].Body, "check_mode: false") {
				t.Errorf("comments = %+v, want one naming the setting and its removal", c)
			}
		})
	}
}
