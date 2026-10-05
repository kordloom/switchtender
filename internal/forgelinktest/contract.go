// Package forgelinktest holds the forgelink.Store contract every backend runs, so the in-memory,
// SQLite, and PostgreSQL stores cannot drift apart on how a link is keyed, which links are refused,
// and who may remove one.
package forgelinktest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/forgelink"
	"github.com/kordloom/switchtender/internal/trigger"
)

// Contract runs the forgelink.Store contract against a fresh store from newStore.
func Contract(t *testing.T, newStore func() forgelink.Store) {
	t.Helper()
	t.Run("a link reads back by the forge's numeric id", func(t *testing.T) {
		testRoundTrip(t, newStore())
	})
	t.Run("the same numeric id on another forge is another account", func(t *testing.T) {
		testKeyedOnForge(t, newStore())
	})
	t.Run("a forge account links to one account", func(t *testing.T) {
		testOneAccountPerForgeAccount(t, newStore())
	})
	t.Run("an account links one account per forge", func(t *testing.T) {
		testOneLinkPerForge(t, newStore())
	})
	t.Run("an account's links are oldest first", func(t *testing.T) { testForUser(t, newStore()) })
	t.Run("only the owning account removes a link", func(t *testing.T) {
		testDeleteByOwner(t, newStore())
	})
	t.Run("deleting an account's links removes all of them", func(t *testing.T) {
		testDeleteUser(t, newStore())
	})
	t.Run("concurrent links of one forge account leave one", func(t *testing.T) {
		testConcurrentCreate(t, newStore())
	})
}

// Forge API bases the contract links accounts on, in canonical form.
const (
	// githubAPI is the public GitHub API.
	githubAPI = "https://api.github.com"
	// enterpriseAPI is a GitHub Enterprise Server API.
	enterpriseAPI = "https://github.example.com/api/v3"
	// gitlabAPI is the public GitLab API.
	gitlabAPI = "https://gitlab.com/api/v4"
)

// base is the instant the contract's links are stamped around, carrying nanoseconds so a store that
// rounds a time is caught.
var base = time.Date(2026, 10, 5, 9, 30, 0, 123456789, time.UTC)

// link returns a link with every field set.
func link(id, userID, provider, apiURL string, forgeUserID int64, at time.Time) *forgelink.Link {
	return &forgelink.Link{ID: id, UserID: userID, Provider: provider, APIURL: apiURL,
		ForgeUserID: forgeUserID, CreatedAt: at}
}

// mustCreate stores l and fails the test when the store refuses it.
func mustCreate(t *testing.T, store forgelink.Store, l *forgelink.Link) {
	t.Helper()
	if err := store.Create(context.Background(), l); err != nil {
		t.Fatalf("Create(%s) error = %v", l.ID, err)
	}
}

// mustLookup reads the link of a forge account and fails the test when there is none.
func mustLookup(t *testing.T, store forgelink.Store, provider, apiURL string,
	forgeUserID int64) *forgelink.Link {
	t.Helper()
	got, err := store.Lookup(context.Background(), provider, apiURL, forgeUserID)
	if err != nil {
		t.Fatalf("Lookup(%s, %s, %d) error = %v", provider, apiURL, forgeUserID, err)
	}
	return got
}

// ids returns the ids of links, in order.
func ids(links []*forgelink.Link) []string {
	out := make([]string, 0, len(links))
	for _, l := range links {
		out = append(out, l.ID)
	}
	return out
}

// testRoundTrip pins that a link reads back exactly as written, its time to the nanosecond, found
// by the forge and the numeric id, and that an unlinked numeric id is ErrNotFound.
func testRoundTrip(t *testing.T, store forgelink.Store) {
	want := link("fl_round", "usr_round", trigger.ProviderGitHub, githubAPI, 583231, base)
	mustCreate(t, store, want)
	got := mustLookup(t, store, trigger.ProviderGitHub, githubAPI, 583231)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Lookup() mismatch (-want +got):\n%s", diff)
	}
	tests := []struct {
		Provider    string
		APIURL      string
		ForgeUserID int64
	}{{ // Test 0: Another numeric id on the same forge.
		Provider: trigger.ProviderGitHub, APIURL: githubAPI, ForgeUserID: 583232,
	}, { // Test 1: The same numeric id under the other provider on an unrelated base.
		Provider: trigger.ProviderGitLab, APIURL: githubAPI, ForgeUserID: 583231,
	}}
	for testNum, test := range tests {
		_, err := store.Lookup(context.Background(), test.Provider, test.APIURL, test.ForgeUserID)
		if !errors.Is(err, forgelink.ErrNotFound) {
			t.Errorf("test %d: Lookup() error = %v, want ErrNotFound", testNum, err)
		}
	}
}

// testKeyedOnForge pins that a numeric id is only an account within its forge: id 7 on GitHub, on
// a GitHub Enterprise Server, and on GitLab are three different people, and each links on its own.
func testKeyedOnForge(t *testing.T, store forgelink.Store) {
	links := []*forgelink.Link{
		link("fl_dotcom", "usr_a", trigger.ProviderGitHub, githubAPI, 7, base),
		link("fl_ghes", "usr_b", trigger.ProviderGitHub, enterpriseAPI, 7, base.Add(time.Second)),
		link("fl_gitlab", "usr_c", trigger.ProviderGitLab, gitlabAPI, 7, base.Add(2*time.Second)),
	}
	for _, l := range links {
		mustCreate(t, store, l)
	}
	for _, want := range links {
		got := mustLookup(t, store, want.Provider, want.APIURL, 7)
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("Lookup(%s, %s) mismatch (-want +got):\n%s", want.Provider, want.APIURL, diff)
		}
	}
}

// testOneAccountPerForgeAccount pins that a forge account acts as exactly one account: a second
// link of it, to anybody, is ErrLinked and leaves the first in place. Without that a comment from
// one forge account could be read as either of two people.
func testOneAccountPerForgeAccount(t *testing.T, store forgelink.Store) {
	first := link("fl_first", "usr_owner", trigger.ProviderGitHub, githubAPI, 42, base)
	mustCreate(t, store, first)
	tests := []struct {
		Link *forgelink.Link
		Want error
	}{{ // Test 0: Another account claims the same forge account.
		Link: link("fl_thief", "usr_other", trigger.ProviderGitHub, githubAPI, 42, base),
		Want: forgelink.ErrLinked,
	}, { // Test 1: The same account links the same forge account again under a new id.
		Link: link("fl_again", "usr_owner", trigger.ProviderGitHub, githubAPI, 42, base),
		Want: forgelink.ErrLinked,
	}, { // Test 2: A link reusing an id already held.
		Link: link("fl_first", "usr_other", trigger.ProviderGitLab, gitlabAPI, 43, base),
		Want: forgelink.ErrLinked,
	}}
	for testNum, test := range tests {
		err := store.Create(context.Background(), test.Link)
		if !errors.Is(err, test.Want) {
			t.Errorf("test %d: Create() error = %v, want %v", testNum, err, test.Want)
		}
	}
	got := mustLookup(t, store, trigger.ProviderGitHub, githubAPI, 42)
	if diff := cmp.Diff(first, got); diff != "" {
		t.Errorf("Lookup() after refused links mismatch (-want +got):\n%s", diff)
	}
	_, err := store.Lookup(context.Background(), trigger.ProviderGitLab, gitlabAPI, 43)
	if !errors.Is(err, forgelink.ErrNotFound) {
		t.Errorf("Lookup() of a refused link error = %v, want ErrNotFound", err)
	}
}

// testOneLinkPerForge pins that an account links one account on each forge, and may link one on
// GitHub and another on GitLab.
func testOneLinkPerForge(t *testing.T, store forgelink.Store) {
	mustCreate(t, store, link("fl_hub", "usr_one", trigger.ProviderGitHub, githubAPI, 100, base))
	err := store.Create(context.Background(),
		link("fl_hub2", "usr_one", trigger.ProviderGitHub, githubAPI, 101, base))
	if !errors.Is(err, forgelink.ErrLinked) {
		t.Errorf("second GitHub link Create() error = %v, want ErrLinked", err)
	}
	mustCreate(t, store, link("fl_lab", "usr_one", trigger.ProviderGitLab, gitlabAPI, 101, base))
	mustCreate(t, store,
		link("fl_ent", "usr_one", trigger.ProviderGitHub, enterpriseAPI, 100, base))
	got, err := store.ForUser(context.Background(), "usr_one")
	if err != nil {
		t.Fatalf("ForUser() error = %v", err)
	}
	if diff := cmp.Diff([]string{"fl_ent", "fl_hub", "fl_lab"}, ids(got)); diff != "" {
		t.Errorf("ForUser() mismatch (-want +got):\n%s", diff)
	}
}

// testForUser pins that an account's links come back oldest first by the instant they were made,
// including within one second, without another account's, and that an account with none gets an
// empty list. The links are made in neither time order nor forge order, so a store that answers in
// the order it wrote them, or in the order of an index over the forge, is caught.
func testForUser(t *testing.T, store forgelink.Store) {
	links := []*forgelink.Link{
		link("fl_b", "usr_list", trigger.ProviderGitHub, enterpriseAPI, 2,
			base.Add(500*time.Millisecond)),
		link("fl_c", "usr_list", trigger.ProviderGitHub, githubAPI, 3, base.Add(time.Second)),
		link("fl_a", "usr_list", trigger.ProviderGitLab, gitlabAPI, 1, base),
		link("fl_x", "usr_else", trigger.ProviderGitHub, githubAPI, 9, base),
	}
	for _, l := range links {
		mustCreate(t, store, l)
	}
	got, err := store.ForUser(context.Background(), "usr_list")
	if err != nil {
		t.Fatalf("ForUser() error = %v", err)
	}
	if diff := cmp.Diff([]string{"fl_a", "fl_b", "fl_c"}, ids(got)); diff != "" {
		t.Errorf("ForUser() order mismatch (-want +got):\n%s", diff)
	}
	none, err := store.ForUser(context.Background(), "usr_nobody")
	if err != nil {
		t.Fatalf("ForUser(none) error = %v", err)
	}
	if diff := cmp.Diff([]*forgelink.Link{}, none, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("ForUser(none) mismatch (-want +got):\n%s", diff)
	}
}

// testDeleteByOwner pins that only the account a link belongs to removes it: another account's
// delete is ErrNotFound and the link stays. Once removed, the forge account may be linked again.
func testDeleteByOwner(t *testing.T, store forgelink.Store) {
	ctx := context.Background()
	want := link("fl_mine", "usr_mine", trigger.ProviderGitHub, githubAPI, 55, base)
	mustCreate(t, store, want)
	if _, err := store.Delete(ctx, "usr_other", "fl_mine"); !errors.Is(err, forgelink.ErrNotFound) {
		t.Errorf("Delete() by another account error = %v, want ErrNotFound", err)
	}
	mustLookup(t, store, trigger.ProviderGitHub, githubAPI, 55)
	got, err := store.Delete(ctx, "usr_mine", "fl_mine")
	if err != nil {
		t.Fatalf("Delete() by the owner error = %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Delete() returned mismatch (-want +got):\n%s", diff)
	}
	if _, err := store.Lookup(ctx, trigger.ProviderGitHub, githubAPI, 55); !errors.Is(err,
		forgelink.ErrNotFound) {
		t.Errorf("Lookup() after delete error = %v, want ErrNotFound", err)
	}
	if _, err := store.Delete(ctx, "usr_mine", "fl_mine"); !errors.Is(err, forgelink.ErrNotFound) {
		t.Errorf("second Delete() error = %v, want ErrNotFound", err)
	}
	mustCreate(t, store, link("fl_relink", "usr_other", trigger.ProviderGitHub, githubAPI, 55, base))
	if got := mustLookup(t, store, trigger.ProviderGitHub, githubAPI, 55); got.UserID != "usr_other" {
		t.Errorf("Lookup() after relinking = %q, want usr_other", got.UserID)
	}
}

// testDeleteUser pins that removing an account's links removes every one of them and none of any
// other account's.
func testDeleteUser(t *testing.T, store forgelink.Store) {
	ctx := context.Background()
	mustCreate(t, store, link("fl_g1", "usr_gone", trigger.ProviderGitHub, githubAPI, 1, base))
	mustCreate(t, store, link("fl_g2", "usr_gone", trigger.ProviderGitLab, gitlabAPI, 1, base))
	mustCreate(t, store, link("fl_k1", "usr_kept", trigger.ProviderGitHub, githubAPI, 2, base))
	tests := []struct {
		UserID    string
		WantCount int
	}{{ // Test 0: Both links of the account are removed.
		UserID: "usr_gone", WantCount: 2,
	}, { // Test 1: Removing them again removes nothing.
		UserID: "usr_gone", WantCount: 0,
	}, { // Test 2: An account with no links.
		UserID: "usr_nobody", WantCount: 0,
	}}
	for testNum, test := range tests {
		got, err := store.DeleteUser(ctx, test.UserID)
		if err != nil {
			t.Fatalf("test %d: DeleteUser() error = %v", testNum, err)
		}
		if got != test.WantCount {
			t.Errorf("test %d: DeleteUser() = %d, want %d", testNum, got, test.WantCount)
		}
	}
	gone, err := store.ForUser(ctx, "usr_gone")
	if err != nil {
		t.Fatalf("ForUser(gone) error = %v", err)
	}
	if diff := cmp.Diff([]*forgelink.Link{}, gone, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("ForUser(gone) mismatch (-want +got):\n%s", diff)
	}
	if got := mustLookup(t, store, trigger.ProviderGitHub, githubAPI, 2); got.UserID != "usr_kept" {
		t.Errorf("Lookup(kept) = %q, want usr_kept", got.UserID)
	}
}

// testConcurrentCreate pins that accounts racing to link one forge account leave exactly one link,
// and every other attempt is ErrLinked, the case of two replicas finishing two sign-ins at once.
func testConcurrentCreate(t *testing.T, store forgelink.Store) {
	const racers = 8
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		won  []string
		errs []error
	)
	for i := range racers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			user := fmt.Sprintf("usr_race_%d", i)
			err := store.Create(context.Background(), link(fmt.Sprintf("fl_race_%d", i), user,
				trigger.ProviderGitHub, githubAPI, 777, base))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won = append(won, user)
			case !errors.Is(err, forgelink.ErrLinked):
				errs = append(errs, err)
			}
		}(i)
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("Create() errors other than ErrLinked: %v", errs)
	}
	if len(won) != 1 {
		t.Fatalf("Create() succeeded for %d of %d racers, want exactly 1: %v", len(won), racers, won)
	}
	if got := mustLookup(t, store, trigger.ProviderGitHub, githubAPI, 777); got.UserID != won[0] {
		t.Errorf("Lookup() = %q, want the winner %q", got.UserID, won[0])
	}
}
