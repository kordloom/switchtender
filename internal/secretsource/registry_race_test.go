package secretsource

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestTheRegistryTakesARegistrationWhileRunsResolve registers dynamic engines while other goroutines
// resolve, list, and validate kinds, the way a plugin registering late meets runs already resolving.
// Under the race detector an unguarded table fails this test, and every engine registered here is
// then found.
func TestTheRegistryTakesARegistrationWhileRunsResolve(t *testing.T) {
	t.Parallel()
	const engines = 8
	// The registry is global and refuses a kind twice, so each run of the test claims fresh names.
	prefix := fmt.Sprintf("registry_race_%d_", time.Now().UnixNano())
	mint := func(context.Context, string) (string, *Lease, error) { return "value", nil, nil }
	var wg sync.WaitGroup
	for i := range engines {
		wg.Add(2)
		go func(n int) {
			defer wg.Done()
			RegisterDynamic(fmt.Sprintf("%s%d", prefix, n), mint)
		}(i)
		go func() {
			defer wg.Done()
			_ = Kinds()
			_ = ValidKind(prefix + "0")
			_ = Registered(prefix + "1")
			_, _, _ = ResolveLeased(context.Background(), prefix+"absent", "")
		}()
	}
	wg.Wait()
	for i := range engines {
		kind := fmt.Sprintf("%s%d", prefix, i)
		value, _, err := ResolveLeased(context.Background(), kind, "")
		if err != nil || value != "value" {
			t.Errorf("ResolveLeased(%q) = %q, %v, want the engine's value", kind, value, err)
		}
	}
}
