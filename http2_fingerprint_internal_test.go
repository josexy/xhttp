// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !nethttpomithttp2

package http

import "testing"

func TestHTTP2FingerprintWriteConfigCachesConnectionKey(t *testing.T) {
	f := Fingerprint{
		Settings:          []Setting{{ID: SettingHeaderTableSize, Val: 65536}},
		HeaderPriority:    &FingerprintHeaderPriority{StreamDep: 3, Weight: 101},
		PseudoHeaderOrder: []string{":method", ":authority", ":scheme", ":path"},
	}
	req, err := NewRequest(MethodGet, "https://example.test/", nil)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := WithRequestFingerprint(req, f)
	if err != nil {
		t.Fatal(err)
	}
	config, ok := http2fingerprintWriteConfigFromContext(bound.Context())
	if !ok {
		t.Fatal("HTTP/2 fingerprint write config missing")
	}
	if got, want := config.connectionKey, http2connectionFingerprintKey(f.http2()); got != want {
		t.Fatalf("connection key = %q; want %q", got, want)
	}
	if allocs := testing.AllocsPerRun(1000, func() {
		cached, ok := http2fingerprintWriteConfigFromContext(bound.Context())
		if !ok || cached.connectionKey == "" {
			panic("connection key missing")
		}
	}); allocs != 0 {
		t.Fatalf("cached connection-key lookup allocated %v times; want 0", allocs)
	}
}
