package secretsource

import (
	"context"
	"errors"
	"testing"
)

func TestResolve(t *testing.T) {
	t.Parallel()
	// Test 0: A local source returns its config unchanged.
	if got, err := Resolve(context.Background(), KindLocal, "plainvalue"); err != nil || got != "plainvalue" {
		t.Errorf("local = %q, %v; want plainvalue", got, err)
	}
	// Test 1: An empty kind is local.
	if got, err := Resolve(context.Background(), "", "plainvalue"); err != nil || got != "plainvalue" {
		t.Errorf("empty kind = %q, %v; want plainvalue", got, err)
	}
	// Test 2: A command source resolves to its stdout.
	if got, err := Resolve(context.Background(), KindCommand, "printf hi"); err != nil || got != "hi" {
		t.Errorf("command = %q, %v; want hi", got, err)
	}
	// Test 3: An unknown kind is an error.
	if _, err := Resolve(context.Background(), "nope", "x"); !errors.Is(err, ErrResolve) {
		t.Errorf("unknown kind error = %v, want ErrResolve", err)
	}
}

func TestResolveCommand(t *testing.T) {
	t.Parallel()
	got, err := resolveCommand(context.Background(), "printf 'hunter2'")
	if err != nil || got != "hunter2" {
		t.Fatalf("resolveCommand() = %q, %v; want hunter2, nil", got, err)
	}
	if _, err := resolveCommand(context.Background(), "exit 7"); !errors.Is(err, ErrResolve) {
		t.Errorf("failing command error = %v, want ErrResolve", err)
	}
}

func TestValidKind(t *testing.T) {
	t.Parallel()
	for _, k := range []string{"", KindLocal, KindCommand, KindVault, KindGSM, KindVaultDynamic} {
		if !ValidKind(k) {
			t.Errorf("ValidKind(%q) = false, want true", k)
		}
	}
	if ValidKind("bogus") {
		t.Error("ValidKind(bogus) = true, want false")
	}
}
