package ui

import (
	"io/fs"
	"testing"
)

// BenchmarkNewAssetHandler measures preparing the embedded asset tree for serving. This runs once
// per process, on the boot path, before the listener opens, so its cost is time an operator waits
// on every restart and its garbage is a collection the server pays for before its first request.
// The allocation figure is the one that matters most: assembling app.js by appending the js parts
// into a buffer that grew as it went cost several times the bundle's own size, so the number is
// benchmarked to keep the sizing in assembleAppJS from being dropped again unnoticed.
func BenchmarkNewAssetHandler(b *testing.B) {
	assets, err := fs.Sub(assetFS, "assets")
	if err != nil {
		b.Fatalf("fs.Sub() error = %v", err)
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = newAssetHandler(assets)
	}
}

// BenchmarkUIHandler measures building the whole web interface handler, which is what the server
// calls at boot: the templates, the route table, and the asset preparation above.
func BenchmarkUIHandler(b *testing.B) {
	u := New(nil, nil, false, 0, false, false, false, "")
	b.ReportAllocs()
	for b.Loop() {
		_ = u.Handler()
	}
}
