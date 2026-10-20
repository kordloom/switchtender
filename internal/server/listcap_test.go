package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
	"github.com/kordloom/switchtender/internal/user"
)

// TestCappedListBoundsTheViews covers the fleet, estate and change views, which still cut at
// maxListRows and say "Showing N of M" rather than paging.
func TestCappedListBoundsTheViews(t *testing.T) {
	t.Parallel()

	big := make([]int, maxListRows*2+7)
	shown, total := cappedList(big)
	if len(shown) != maxListRows {
		t.Errorf("shown = %d rows, want the cap of %d", len(shown), maxListRows)
	}
	if total != len(big) {
		t.Errorf("total = %d, want %d: a caller shown a prefix has to be told of what",
			total, len(big))
	}

	// Every ordinary install is under the cap, where Count and Total agree and nothing is cut.
	small := make([]int, 12)
	shown, total = cappedList(small)
	if len(shown) != 12 || total != 12 {
		t.Errorf("small install: shown %d total %d, want 12 and 12", len(shown), total)
	}

	// Exactly at the cap is not truncated, so no install is told it is missing rows it is not.
	exact := make([]int, maxListRows)
	shown, total = cappedList(exact)
	if len(shown) != maxListRows || total != maxListRows {
		t.Errorf("at the cap: shown %d total %d, want %d twice", len(shown), total, maxListRows)
	}

	// An empty list stays empty rather than becoming nil-with-a-count.
	shown, total = cappedList([]int{})
	if len(shown) != 0 || total != 0 {
		t.Errorf("empty list: shown %d total %d, want 0 and 0", len(shown), total)
	}
}

// listRowID names the i'th row of a list fixture, so the store's order is the ids' order.
func listRowID(i int) string {
	return fmt.Sprintf("obj_%05d", i)
}

// TestPageList pins how pageList reads limit, offset and id over a list of 1001 rows.
func TestPageList(t *testing.T) {
	t.Parallel()
	all := make([]string, maxListRows+1)
	for i := range all {
		all[i] = listRowID(i)
	}
	tests := []struct {
		Query          string
		WantFirst      string
		WantLen        int
		WantTotal      int
		WantHasMore    bool
		WantNextOffset int
	}{{ // Test 0: No parameters reads the first full page.
		Query: "", WantFirst: listRowID(0), WantLen: maxListRows, WantTotal: maxListRows + 1,
		WantHasMore: true, WantNextOffset: maxListRows,
	}, { // Test 1: The next offset reads the last row and says nothing follows.
		Query: "offset=1000", WantFirst: listRowID(1000), WantLen: 1, WantTotal: maxListRows + 1,
		WantHasMore: false, WantNextOffset: maxListRows + 1,
	}, { // Test 2: A limit over the maximum is clamped to it.
		Query: "limit=5000", WantFirst: listRowID(0), WantLen: maxListRows, WantTotal: maxListRows + 1,
		WantHasMore: true, WantNextOffset: maxListRows,
	}, { // Test 3: A small limit pages in small steps.
		Query: "limit=10&offset=20", WantFirst: listRowID(20), WantLen: 10, WantTotal: maxListRows + 1,
		WantHasMore: true, WantNextOffset: 30,
	}, { // Test 4: A zero, negative or non-numeric limit is the default.
		Query: "limit=-4&offset=abc", WantFirst: listRowID(0), WantLen: maxListRows,
		WantTotal: maxListRows + 1, WantHasMore: true, WantNextOffset: maxListRows,
	}, { // Test 5: A negative offset reads as zero.
		Query: "offset=-7&limit=3", WantFirst: listRowID(0), WantLen: 3, WantTotal: maxListRows + 1,
		WantHasMore: true, WantNextOffset: 3,
	}, { // Test 6: An offset past the end is an empty last page.
		Query: "offset=99999", WantLen: 0, WantTotal: maxListRows + 1, WantHasMore: false,
		WantNextOffset: maxListRows + 1,
	}, { // Test 7: A limit ending exactly at the last row says nothing follows.
		Query: "limit=1&offset=1000", WantFirst: listRowID(1000), WantLen: 1,
		WantTotal: maxListRows + 1, WantHasMore: false, WantNextOffset: maxListRows + 1,
	}, { // Test 8: Repeated id narrows to the named rows, in list order, before paging.
		Query: "id=" + listRowID(1000) + "&id=" + listRowID(3), WantFirst: listRowID(3), WantLen: 2,
		WantTotal: 2, WantHasMore: false, WantNextOffset: 2,
	}, { // Test 9: An id that names nothing reads an empty list.
		Query: "id=obj_missing", WantLen: 0, WantTotal: 0, WantHasMore: false, WantNextOffset: 0,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "/v1/x?"+test.Query, nil)
			shown, total, hasMore, next := pageList(req, all, func(s string) string { return s })
			first := ""
			if len(shown) > 0 {
				first = shown[0]
			}
			got := []any{first, len(shown), total, hasMore, next}
			want := []any{test.WantFirst, test.WantLen, test.WantTotal, test.WantHasMore,
				test.WantNextOffset}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("first, len, total, has_more, next_offset mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// listFixtureStores answers List from fixed slices, so one list handler can be driven with any
// number of rows. Each embeds its store interface and leaves the methods a list does not call unset.
type (
	// listTokens is an auth.Store over a fixed token list.
	listTokens struct {
		auth.Store
		// rows is the list List returns.
		rows []*auth.Token
	}
	// listNotifications is a notification.Store over a fixed target list with no deliveries.
	listNotifications struct {
		notification.Store
		// rows is the list List returns.
		rows []*notification.Notification
	}
	// listSchedules is a schedule.Store over a fixed schedule list.
	listSchedules struct {
		schedule.Store
		// rows is the list List returns.
		rows []*schedule.Schedule
	}
	// listOrgs is an org.Store over a fixed organization list, each with the same members.
	listOrgs struct {
		org.Store
		// rows is the list List returns.
		rows []*org.Org
		// members is the list Members returns for any organization.
		members []org.Member
	}
	// listUsers is a user.Store over a fixed account list.
	listUsers struct {
		user.Store
		// rows is the list List returns.
		rows []*user.User
	}
	// listTriggers is a trigger.Store over a fixed trigger list.
	listTriggers struct {
		trigger.Store
		// rows is the list List returns.
		rows []*trigger.Trigger
	}
	// listTemplates is a template.Store over a fixed template list with no AWX bindings.
	listTemplates struct {
		template.Store
		// rows is the list List returns.
		rows []*template.Template
	}
)

// List returns the fixed tokens.
func (s *listTokens) List(context.Context) ([]*auth.Token, error) { return s.rows, nil }

// List returns the fixed targets.
func (s *listNotifications) List(context.Context) ([]*notification.Notification, error) {
	return s.rows, nil
}

// Deliveries returns none, so every target reads as never delivered to.
func (s *listNotifications) Deliveries(context.Context,
	notification.DeliveryFilter) ([]*notification.Delivery, error) {
	return nil, nil
}

// List returns the fixed schedules.
func (s *listSchedules) List(context.Context) ([]*schedule.Schedule, error) { return s.rows, nil }

// List returns the fixed organizations.
func (s *listOrgs) List(context.Context) ([]*org.Org, error) { return s.rows, nil }

// Get returns an organization for any id, so the members route always finds one.
func (s *listOrgs) Get(_ context.Context, id string) (*org.Org, error) {
	return &org.Org{ID: id}, nil
}

// Members returns the fixed members.
func (s *listOrgs) Members(context.Context, string) ([]org.Member, error) { return s.members, nil }

// List returns the fixed accounts.
func (s *listUsers) List(context.Context) ([]*user.User, error) { return s.rows, nil }

// List returns the fixed triggers.
func (s *listTriggers) List(context.Context) ([]*trigger.Trigger, error) { return s.rows, nil }

// List returns the fixed templates.
func (s *listTemplates) List(context.Context) ([]*template.Template, error) { return s.rows, nil }

// AWXBindings returns none.
func (s *listTemplates) AWXBindings(context.Context) ([]template.AWXBinding, error) {
	return nil, nil
}

// pagedListCase is one configuration list endpoint driven with n rows.
type pagedListCase struct {
	// Name labels the endpoint.
	Name string
	// Path is the route the request is made to, including any path value.
	Path string
	// Key is the response field holding the rows.
	Key string
	// IDField is the row field holding its object id.
	IDField string
	// Filtered reports whether the handler applies a read filter, so a restricted reader sees less.
	Filtered bool
	// Handler builds the handler over n rows whose ids are listRowID(0) to listRowID(n-1), each
	// row's template named by its own id where a row has one, under authz.
	Handler func(n int, authz *authorizer) http.HandlerFunc
}

// pagedListCases returns the eight configuration lists that page.
func pagedListCases() []pagedListCase {
	created := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	nop := zap.NewNop()
	return []pagedListCase{{
		Name: "tokens", Path: "/v1/tokens", Key: "tokens", IDField: "id",
		Handler: func(n int, _ *authorizer) http.HandlerFunc {
			s := &listTokens{}
			for i := range n {
				s.rows = append(s.rows, &auth.Token{ID: listRowID(i), CreatedAt: created})
			}
			return listTokensHandler(s, nop)
		},
	}, {
		Name: "notification targets", Path: "/v1/notifications", Key: "notifications", IDField: "id",
		Filtered: true,
		Handler: func(n int, authz *authorizer) http.HandlerFunc {
			s := &listNotifications{}
			for i := range n {
				s.rows = append(s.rows, &notification.Notification{ID: listRowID(i), CreatedAt: created})
			}
			return listNotificationsHandler(s, nil, authz, nop)
		},
	}, {
		Name: "schedules", Path: "/v1/schedules", Key: "schedules", IDField: "id", Filtered: true,
		Handler: func(n int, authz *authorizer) http.HandlerFunc {
			s := &listSchedules{}
			for i := range n {
				s.rows = append(s.rows, &schedule.Schedule{ID: listRowID(i), TemplateID: listRowID(i),
					CreatedAt: created})
			}
			return listSchedulesHandler(s, authz, nop)
		},
	}, {
		Name: "organizations", Path: "/v1/orgs", Key: "orgs", IDField: "id",
		Handler: func(n int, _ *authorizer) http.HandlerFunc {
			s := &listOrgs{}
			for i := range n {
				s.rows = append(s.rows, &org.Org{ID: listRowID(i), CreatedAt: created})
			}
			return listOrgsHandler(s, nop)
		},
	}, {
		Name: "organization members", Path: "/v1/orgs/org_1/members", Key: "members",
		IDField: "user_id",
		Handler: func(n int, _ *authorizer) http.HandlerFunc {
			s := &listOrgs{}
			for i := range n {
				s.members = append(s.members, org.Member{UserID: listRowID(i), Role: org.RoleMember})
			}
			mux := http.NewServeMux()
			mux.Handle("GET /v1/orgs/{id}/members", listOrgMembersHandler(s, nop))
			return mux.ServeHTTP
		},
	}, {
		Name: "templates", Path: "/v1/templates", Key: "templates", IDField: "id", Filtered: true,
		Handler: func(n int, authz *authorizer) http.HandlerFunc {
			s := &listTemplates{}
			for i := range n {
				s.rows = append(s.rows, &template.Template{ID: listRowID(i), Playbook: "site.yml",
					CreatedAt: created})
			}
			return listTemplatesHandler(s, authz, nop)
		},
	}, {
		Name: "users", Path: "/v1/users", Key: "users", IDField: "id",
		Handler: func(n int, _ *authorizer) http.HandlerFunc {
			s := &listUsers{}
			for i := range n {
				s.rows = append(s.rows, &user.User{ID: listRowID(i), Username: listRowID(i),
					Role: user.RoleViewer, CreatedAt: created})
			}
			return listUsersHandler(s, nop)
		},
	}, {
		Name: "triggers", Path: "/v1/triggers", Key: "triggers", IDField: "id", Filtered: true,
		Handler: func(n int, authz *authorizer) http.HandlerFunc {
			s := &listTriggers{}
			for i := range n {
				s.rows = append(s.rows, &trigger.Trigger{ID: listRowID(i), TemplateID: listRowID(i),
					CreatedAt: created})
			}
			return listTriggersHandler(s, authz, nop)
		},
	}}
}

// listPageAnswer is the paging part of a configuration list response and its row ids.
type listPageAnswer struct {
	// IDs are the ids of the rows on the page, in order.
	IDs []string
	// Count is the response's count.
	Count int
	// Total is the response's total.
	Total int
	// HasMore is the response's has_more.
	HasMore bool
	// NextOffset is the response's next_offset.
	NextOffset int
}

// readListPage requests query from the case's handler as actor and decodes the page.
func readListPage(t *testing.T, c pagedListCase, h http.HandlerFunc, actor Actor,
	query string) listPageAnswer {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, c.Path+"?"+query, nil).
		WithContext(context.WithValue(context.Background(), actorKey{}, actor))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s %s: status %d: %s", c.Name, query, rec.Code, rec.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: decode: %v", c.Name, err)
	}
	var rows []map[string]any
	var ans listPageAnswer
	for key, into := range map[string]any{c.Key: &rows, "count": &ans.Count, "total": &ans.Total,
		"has_more": &ans.HasMore, "next_offset": &ans.NextOffset} {
		raw, ok := body[key]
		if !ok {
			t.Fatalf("%s: response has no %q field: %s", c.Name, key, rec.Body.String())
		}
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatalf("%s: decode %q: %v", c.Name, key, err)
		}
	}
	for _, row := range rows {
		id, _ := row[c.IDField].(string)
		ans.IDs = append(ans.IDs, id)
	}
	return ans
}

// TestConfigurationListsPage drives every configuration list with 1001 rows and checks that the
// 1001st is reachable, that id reads exactly the named rows, and that the limit is clamped.
//
// The lists used to cut at 1000 rows with no way to ask for more, so row 1001 existed in the store
// and nowhere in the API, the UI, or the agent tools.
func TestConfigurationListsPage(t *testing.T) {
	t.Parallel()
	admin := Actor{UserID: "user_admin", Role: user.RoleAdmin}
	n := maxListRows + 1
	for _, c := range pagedListCases() {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			h := c.Handler(n, &authorizer{strict: true, grants: &fakeGrants{}})

			first := readListPage(t, c, h, admin, "")
			if len(first.IDs) != maxListRows || first.Count != maxListRows || first.Total != n ||
				!first.HasMore || first.NextOffset != maxListRows {
				t.Errorf("page 1: %d rows, count %d, total %d, has_more %v, next_offset %d, want "+
					"%d, %d, %d, true, %d", len(first.IDs), first.Count, first.Total, first.HasMore,
					first.NextOffset, maxListRows, maxListRows, n, maxListRows)
			}
			second := readListPage(t, c, h, admin, fmt.Sprintf("offset=%d", first.NextOffset))
			want := listPageAnswer{IDs: []string{listRowID(maxListRows)}, Count: 1, Total: n,
				HasMore: false, NextOffset: n}
			if diff := cmp.Diff(want, second); diff != "" {
				t.Errorf("page 2 must hold row 1001 and end the list (-want +got):\n%s", diff)
			}

			named := readListPage(t, c, h, admin,
				"id="+listRowID(maxListRows)+"&id="+listRowID(7)+"&id=obj_missing")
			want = listPageAnswer{IDs: []string{listRowID(7), listRowID(maxListRows)}, Count: 2,
				Total: 2, HasMore: false, NextOffset: 2}
			if diff := cmp.Diff(want, named); diff != "" {
				t.Errorf("id must read exactly the named rows (-want +got):\n%s", diff)
			}

			clamped := readListPage(t, c, h, admin, "limit=100000&offset=-3")
			if len(clamped.IDs) != maxListRows || clamped.IDs[0] != listRowID(0) {
				t.Errorf("limit 100000 offset -3: %d rows from %v, want %d from row 0",
					len(clamped.IDs), clamped.IDs[:min(1, len(clamped.IDs))], maxListRows)
			}
		})
	}
}

// TestRestrictedReaderPagesOverReadableRows checks that a strict-grants reader's offsets and total
// count only the rows it may read.
//
// Paging the store's list and then filtering would hand a restricted reader short pages whose
// offsets run over rows it cannot see, so walking next_offset either repeats or skips rows, and
// total would publish how many objects the install holds beyond the caller's reach.
func TestRestrictedReaderPagesOverReadableRows(t *testing.T) {
	t.Parallel()
	const n = 2*maxListRows + 50
	reader := Actor{UserID: "user_reader", Role: user.RoleOperator}
	grants := &fakeGrants{byObject: map[string][]*grant.Grant{}}
	var readable []string
	for i := range n {
		if i%7 == 3 {
			continue
		}
		grants.byObject[listRowID(i)] = []*grant.Grant{{Subject: reader.UserID,
			Access: grant.AccessUse}}
		readable = append(readable, listRowID(i))
	}
	for _, c := range pagedListCases() {
		if !c.Filtered {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			h := c.Handler(n, &authorizer{strict: true, grants: grants})
			var got []string
			offset, pages := 0, 0
			for {
				page := readListPage(t, c, h, reader, fmt.Sprintf("offset=%d", offset))
				pages++
				if page.Total != len(readable) {
					t.Fatalf("page %d: total %d, want %d readable rows", pages, page.Total,
						len(readable))
				}
				if page.HasMore && len(page.IDs) != maxListRows {
					t.Errorf("page %d holds %d rows with more to come, want a full page of %d",
						pages, len(page.IDs), maxListRows)
				}
				got = append(got, page.IDs...)
				if !page.HasMore || pages > 10 {
					break
				}
				offset = page.NextOffset
			}
			if diff := cmp.Diff(readable, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("walking every page must read each readable row once (-want +got):\n%s",
					diff)
			}
		})
	}
}
