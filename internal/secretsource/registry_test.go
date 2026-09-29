package secretsource

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// wantPanic runs fn and reports whether it panicked. The registry panics on a duplicate or reserved
// kind rather than overwriting one, so the refusal has to be observed as a panic.
func wantPanic(t *testing.T, fn func()) (panicked bool) {
	t.Helper()
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	fn()
	return false
}

// TestNormalizeKind pins the one mapping every caller depends on: an unset source is local, and
// nothing else is rewritten. A kind that normalized loosely, by trimming or lowercasing, would let
// two spellings of the same name reach different resolvers, so the exactness matters.
func TestNormalizeKind(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the kind as stored on the source.
		In string
		// WantKind is the normalized kind.
		WantKind string
	}{{ // Test 0: An empty kind is the local default.
		In: "", WantKind: KindLocal,
	}, { // Test 1: The local kind is unchanged.
		In: KindLocal, WantKind: KindLocal,
	}, { // Test 2: A registered kind is unchanged.
		In: KindVault, WantKind: KindVault,
	}, { // Test 3: Case is not folded, so LOCAL is not the local kind.
		In: "LOCAL", WantKind: "LOCAL",
	}, { // Test 4: Surrounding space is not trimmed.
		In: " local ", WantKind: " local ",
	}, { // Test 5: An unknown kind passes through for ValidKind to refuse.
		In: "not-a-kind", WantKind: "not-a-kind",
	}, { // Test 6: Unicode passes through unchanged.
		In: "vаult", WantKind: "vаult",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantKind, NormalizeKind(test.In)); diff != "" {
				t.Errorf("NormalizeKind mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestValidKindRefusesAnythingUnregistered proves the kind check fails closed. ValidKind gates
// which sources an operator may store, so a near miss such as a case variant or a padded name has
// to be refused rather than quietly accepted and then failing at resolve time with an unknown
// source.
func TestValidKindRefusesAnythingUnregistered(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the candidate kind.
		In string
		// WantValid is whether the kind names a supported source.
		WantValid bool
	}{{ // Test 0: An empty kind is the local default and is valid.
		In: "", WantValid: true,
	}, { // Test 1: Local is valid.
		In: KindLocal, WantValid: true,
	}, { // Test 2: A registered resolver is valid.
		In: KindOnePassword, WantValid: true,
	}, { // Test 3: A registered dynamic engine is valid.
		In: KindAWSSTS, WantValid: true,
	}, { // Test 4: An uppercase spelling of a real kind is not registered.
		In: "VAULT", WantValid: false,
	}, { // Test 5: A padded spelling of a real kind is not registered.
		In: " vault", WantValid: false,
	}, { // Test 6: A trailing newline makes it a different kind.
		In: "vault\n", WantValid: false,
	}, { // Test 7: A prefix of a real kind is not registered.
		In: "vaul", WantValid: false,
	}, { // Test 8: An extension of a real kind is not registered.
		In: "vault_dynamic_extra", WantValid: false,
	}, { // Test 9: A nul byte does not truncate the lookup.
		In: "vault\x00", WantValid: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := ValidKind(test.In); got != test.WantValid {
				t.Errorf("ValidKind(%q) = %v, want %v", test.In, got, test.WantValid)
			}
		})
	}
}

// TestRegisteredMatchesValidKind pins that the plugin loader's namespace check and the source
// validator agree. The loader refuses a plugin whose kind Registered already claims, and the
// registry panics on a duplicate, so a disagreement between the two would take the server down at
// plugin load instead of rejecting the plugin.
func TestRegisteredMatchesValidKind(t *testing.T) {
	t.Parallel()
	kinds := []string{
		"", KindLocal, KindCommand, KindVault, KindGSM, KindAWS, KindAzure, KindConjur, KindCCP,
		KindOnePassword, KindVaultDynamic, KindAWSSTS, "unclaimed-kind", "LOCAL", " local",
	}
	for testNum, kind := range kinds {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got, want := Registered(kind), ValidKind(kind); got != want {
				t.Errorf("Registered(%q) = %v but ValidKind = %v; the plugin loader and the "+
					"source validator disagree, so a plugin the loader admits would panic the registry",
					kind, got, want)
			}
		})
	}
}

// TestKindsIsTheExactSetValidKindAccepts pins the promise in the Kinds doc comment. The user-facing
// hint listing valid sources is built from Kinds, so an entry Kinds omits is a source an operator
// is never told about, and an entry ValidKind refuses is a source the hint invites them to fail
// with.
func TestKindsIsTheExactSetValidKindAccepts(t *testing.T) {
	t.Parallel()
	got := Kinds()

	// Every listed kind is one ValidKind and Registered accept.
	for _, k := range got {
		if !ValidKind(k) {
			t.Errorf("Kinds lists %q but ValidKind refuses it", k)
		}
		if !Registered(k) {
			t.Errorf("Kinds lists %q but Registered says it is unclaimed", k)
		}
	}

	// Every built-in kind is listed, so the hint cannot drift from the resolver and minter tables.
	want := []string{
		KindLocal, KindCommand, KindVault, KindGSM, KindAWS, KindAzure, KindConjur, KindCCP,
		KindOnePassword, KindVaultDynamic, KindAWSSTS,
	}
	index := make(map[string]bool, len(got))
	for _, k := range got {
		index[k] = true
	}
	for _, k := range want {
		if !index[k] {
			t.Errorf("Kinds omits the built-in kind %q", k)
		}
	}

	// The list is sorted and free of duplicates, so a resolver and a minter can never share a name.
	if diff := cmp.Diff(len(index), len(got)); diff != "" {
		t.Errorf("Kinds returned a duplicate entry (-uniques +returned):\n%s", diff)
	}
	sorted := append([]string(nil), got...)
	sort.Strings(sorted)
	if diff := cmp.Diff(sorted, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("Kinds is not sorted (-sorted +got):\n%s", diff)
	}
	if len(got) == 0 {
		t.Error("Kinds returned nothing, so the source hint would be empty")
	}
}

// TestRegisterInstallsTheResolverItWasGiven checks the wiring rather than the rule. A Register that
// stored the wrong function, or stored it under a different name, would still pass every test of
// the resolvers themselves, so the only way to catch it is to resolve through the kind that was
// registered and see the registered function's own output come back.
//
// It mutates the process-wide registry, so it does not run in parallel.
func TestRegisterInstallsTheResolverItWasGiven(t *testing.T) {
	const kind = "registry-test-resolver"
	if Registered(kind) {
		t.Fatalf("precondition: %q is already registered", kind)
	}

	var gotConfig string
	Register(kind, func(_ context.Context, config string) (string, error) {
		gotConfig = config
		return "resolved:" + config, nil
	})

	// The registry now claims the kind by every route callers use.
	if !Registered(kind) || !ValidKind(kind) {
		t.Errorf("after Register: Registered = %v, ValidKind = %v; want both true",
			Registered(kind), ValidKind(kind))
	}
	found := false
	for _, k := range Kinds() {
		if k == kind {
			found = true
		}
	}
	if !found {
		t.Errorf("Kinds does not list the newly registered %q", kind)
	}

	// Resolving through the kind reaches that exact function, with the config verbatim.
	value, lease, err := ResolveLeased(context.Background(), kind, "cfg-value")
	if err != nil {
		t.Fatalf("ResolveLeased through a registered kind: %v", err)
	}
	if diff := cmp.Diff("resolved:cfg-value", value); diff != "" {
		t.Errorf("value mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff("cfg-value", gotConfig); diff != "" {
		t.Errorf("config passed to the resolver mismatch (-want +got):\n%s", diff)
	}
	if lease != nil {
		t.Errorf("a plain resolver returned lease %v, want none; only a dynamic engine leases", lease)
	}
}

// TestRegisterDynamicInstallsTheMinterItWasGiven checks the dynamic half of the same wiring. A
// minter filed under the wrong name, or a lease built for the wrong engine, would leave a
// short-lived credential unrevoked at the end of a run, so the lease has to come back naming this
// engine and revoking through this function.
//
// It mutates the process-wide registry, so it does not run in parallel.
func TestRegisterDynamicInstallsTheMinterItWasGiven(t *testing.T) {
	const kind = "registry-test-minter"
	if Registered(kind) {
		t.Fatalf("precondition: %q is already registered", kind)
	}

	revoked := 0
	RegisterDynamic(kind, func(_ context.Context, config string) (string, *Lease, error) {
		return "minted:" + config, NewLease(kind, func(context.Context) error {
			revoked++
			return nil
		}), nil
	})

	if !Registered(kind) || !ValidKind(kind) {
		t.Errorf("after RegisterDynamic: Registered = %v, ValidKind = %v; want both true",
			Registered(kind), ValidKind(kind))
	}

	value, lease, err := ResolveLeased(context.Background(), kind, "role")
	if err != nil {
		t.Fatalf("ResolveLeased through a dynamic kind: %v", err)
	}
	if diff := cmp.Diff("minted:role", value); diff != "" {
		t.Errorf("value mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(kind, lease.Kind()); diff != "" {
		t.Errorf("lease kind mismatch (-want +got):\n%s", diff)
	}
	if err := lease.Revoke(context.Background()); err != nil {
		t.Errorf("Revoke() = %v, want nil", err)
	}
	if revoked != 1 {
		t.Errorf("revoke ran %d times, want 1; a minted credential outlives the run if it never runs",
			revoked)
	}

	// Resolve discards the lease but still returns the minted value, as its doc comment promises.
	plain, err := Resolve(context.Background(), kind, "role")
	if err != nil || plain != "minted:role" {
		t.Errorf("Resolve through a dynamic kind = %q, %v; want minted:role", plain, err)
	}
}

// TestRegisterRefusesAReservedOrClaimedKind proves the registry fails closed on a name collision.
// Resolvers and minters share one namespace, so silently overwriting an entry would repoint an
// existing source at a plugin's code and send the secrets of every source of that kind somewhere
// new. A panic at load is the intended outcome, which is why the plugin loader checks Registered
// first.
//
// It touches the process-wide registry, so it does not run in parallel.
//
//nolint:funlen // Test function.
func TestRegisterRefusesAReservedOrClaimedKind(t *testing.T) {
	const takenResolver = "registry-test-taken-resolver"
	const takenMinter = "registry-test-taken-minter"
	Register(takenResolver, func(context.Context, string) (string, error) { return "", nil })
	RegisterDynamic(takenMinter, func(context.Context, string) (string, *Lease, error) {
		return "", nil, nil
	})

	noopResolver := func(context.Context, string) (string, error) { return "", nil }
	noopMinter := func(context.Context, string) (string, *Lease, error) { return "", nil, nil }

	tests := []struct {
		// Register attempts the registration that must be refused.
		Register func()
		// Why names the collision, for the failure message.
		Why string
	}{{ // Test 0: An empty kind is reserved, since it normalizes to local.
		Why: "an empty resolver kind", Register: func() { Register("", noopResolver) },
	}, { // Test 1: The local kind is reserved.
		Why: "the local resolver kind", Register: func() { Register(KindLocal, noopResolver) },
	}, { // Test 2: A built-in resolver kind cannot be taken over.
		Why: "a built-in resolver kind", Register: func() { Register(KindVault, noopResolver) },
	}, { // Test 3: A resolver kind registered earlier cannot be taken over.
		Why: "an already registered resolver", Register: func() { Register(takenResolver, noopResolver) },
	}, { // Test 4: A resolver cannot take a name a minter already holds.
		Why: "a name held by a minter", Register: func() { Register(takenMinter, noopResolver) },
	}, { // Test 5: An empty dynamic kind is reserved.
		Why: "an empty dynamic kind", Register: func() { RegisterDynamic("", noopMinter) },
	}, { // Test 6: The local kind is reserved for dynamic engines too.
		Why: "the local dynamic kind", Register: func() { RegisterDynamic(KindLocal, noopMinter) },
	}, { // Test 7: A built-in dynamic kind cannot be taken over.
		Why: "a built-in dynamic kind", Register: func() { RegisterDynamic(KindAWSSTS, noopMinter) },
	}, { // Test 8: A dynamic kind registered earlier cannot be taken over.
		Why:      "an already registered minter",
		Register: func() { RegisterDynamic(takenMinter, noopMinter) },
	}, { // Test 9: A minter cannot take a name a resolver already holds.
		Why: "a name held by a resolver", Register: func() { RegisterDynamic(takenResolver, noopMinter) },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			if !wantPanic(t, test.Register) {
				t.Errorf("registering %s did not panic; the existing entry would be silently "+
					"replaced and every source of that kind would resolve through the new code",
					test.Why)
			}
		})
	}

	// The refusals left the registry as it was: the built-in kinds still resolve to their own code.
	local, err := Resolve(context.Background(), KindLocal, "still-local")
	if err != nil || local != "still-local" {
		t.Errorf("after the refused registrations, local = %q, %v; want still-local", local, err)
	}
	if got, err := Resolve(context.Background(), KindCommand, "printf ok"); err != nil || got != "ok" {
		t.Errorf("after the refused registrations, command = %q, %v; want ok", got, err)
	}
}

// TestLeaseHandlesTheNilAndNoRevokeCases pins that a caller can revoke unconditionally. Revocation
// runs in the cleanup path after a run, where the lease may be absent because the source was local
// or because the mint failed, and a panic there would abandon the rest of the cleanup.
func TestLeaseHandlesTheNilAndNoRevokeCases(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("revoke refused")
	tests := []struct {
		// Lease is the lease under test.
		Lease *Lease
		// WantKind is the engine name the lease reports.
		WantKind string
		// Want is the error Revoke returns.
		Want error
	}{{ // Test 0: A nil lease names no engine and revokes as a no-op.
		Lease: nil, WantKind: "", Want: nil,
	}, { // Test 1: A lease with no revoke func names its engine and revokes as a no-op.
		Lease: NewLease(KindAWSSTS, nil), WantKind: KindAWSSTS, Want: nil,
	}, { // Test 2: A lease with a revoke func returns that func's error unchanged.
		Lease:    NewLease(KindVaultDynamic, func(context.Context) error { return sentinel }),
		WantKind: KindVaultDynamic, Want: sentinel,
	}, { // Test 3: An empty engine name is carried as given rather than defaulted.
		Lease: NewLease("", nil), WantKind: "", Want: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantKind, test.Lease.Kind()); diff != "" {
				t.Errorf("Kind mismatch (-want +got):\n%s", diff)
			}
			if err := test.Lease.Revoke(context.Background()); !errors.Is(err, test.Want) {
				t.Errorf("Revoke() = %v, want %v", err, test.Want)
			}
		})
	}
}

// TestResolveLeasedReturnsNoLeaseWhenAMintFails pins that a failed mint hands back nothing to
// revoke. A lease returned alongside an error would be revoked by the cleanup path against a
// credential that was never minted, and for Vault that is an authenticated request built from an
// unvalidated config.
func TestResolveLeasedReturnsNoLeaseWhenAMintFails(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Kind is the dynamic engine under test.
		Kind string
		// Config is a config that cannot mint.
		Config string
	}{{ // Test 0: Vault dynamic with unparseable config JSON.
		Kind: KindVaultDynamic, Config: "{not json",
	}, { // Test 1: Vault dynamic with no addr, path, or field.
		Kind: KindVaultDynamic, Config: "{}",
	}, { // Test 2: AWS STS with unparseable config JSON.
		Kind: KindAWSSTS, Config: "{not json",
	}, { // Test 3: AWS STS with no role ARN.
		Kind: KindAWSSTS, Config: "{}",
	}, { // Test 4: An unknown kind resolves to nothing at all.
		Kind: "no-such-engine", Config: "{}",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			value, lease, err := ResolveLeased(context.Background(), test.Kind, test.Config)
			if !errors.Is(err, ErrResolve) {
				t.Fatalf("error = %v, want ErrResolve", err)
			}
			if value != "" {
				t.Errorf("value = %q, want empty on a failed mint", value)
			}
			if lease != nil {
				t.Errorf("lease = %v, want none; cleanup would revoke a credential never minted", lease)
			}
		})
	}
}
