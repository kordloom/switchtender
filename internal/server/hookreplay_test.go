package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHookDeliveryKeysOnSignedMaterial checks a signed webhook cannot be replayed into a second run
// by changing a header.
//
// The dedupe key came from X-GitHub-Delivery, which sits outside the signed body. A repository admin
// holding no SwitchTender account can open the forge's "Recent Deliveries" pane, which shows the full
// body and its signature header, resend both with a fresh delivery id, and launch a real deployment
// again. The signature verified every time, because it was never the thing being replayed.
func TestHookDeliveryKeysOnSignedMaterial(t *testing.T) {
	t.Parallel()
	body := []byte(`{"ref":"refs/heads/main","after":"c0ffee"}`)

	withHeader := func(id string) string {
		r := httptest.NewRequest("POST", "/hooks/tok", nil)
		r.Header.Set("X-GitHub-Delivery", id)
		return hookDelivery(r, body)
	}

	// The same signed body under two different delivery ids is one event, not two.
	first, second := withHeader("delivery-1"), withHeader("delivery-2")
	if first != second {
		t.Errorf("the same signed body keyed differently under two delivery ids: %q and %q",
			first, second)
	}
	if first == "" {
		t.Error("a signed body produced no dedupe key, so nothing collapses a redelivery")
	}

	// A genuinely different event keys differently, or every push after the first would be swallowed.
	other := hookDelivery(httptest.NewRequest("POST", "/hooks/tok", nil),
		[]byte(`{"ref":"refs/heads/main","after":"deadbee"}`))
	if other == first {
		t.Error("two different payloads share a dedupe key, so the second would never run")
	}

	// With no signature there is nothing trustworthy to key on, so the header is still used.
	unsignedA := hookDelivery(headerReq(t, "X-GitHub-Delivery", "delivery-1"), nil)
	unsignedB := hookDelivery(headerReq(t, "X-GitHub-Delivery", "delivery-2"), nil)
	if unsignedA == "" || unsignedA == unsignedB {
		t.Errorf("unsigned deliveries lost their per-delivery key: %q and %q", unsignedA, unsignedB)
	}

	// A sender that offers nothing at all still gets the bounded time-bucket fallback.
	if got := hookDelivery(httptest.NewRequest("POST", "/hooks/tok", nil), nil); got != "" {
		t.Errorf("a delivery with nothing to identify it produced %q, want the empty fallback", got)
	}
}

// headerReq builds a request carrying one header, for the unsigned cases above.
func headerReq(t *testing.T, name, value string) *http.Request {
	t.Helper()
	r := httptest.NewRequest("POST", "/hooks/tok", nil)
	r.Header.Set(name, value)
	return r
}
