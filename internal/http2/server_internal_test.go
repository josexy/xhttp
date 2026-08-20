// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestCheckValidHTTP2Request(t *testing.T) {
	tests := []struct {
		h    Header
		want error
	}{
		{
			h:    Header{"Te": {"trailers"}},
			want: nil,
		},
		{
			h:    Header{"Te": {"trailers", "bogus"}},
			want: errors.New(`request header "TE" may only be "trailers" in HTTP/2`),
		},
		{
			h:    Header{"Foo": {""}},
			want: nil,
		},
		{
			h:    Header{"Connection": {""}},
			want: errors.New(`request header "Connection" is not valid in HTTP/2`),
		},
		{
			h:    Header{"Proxy-Connection": {""}},
			want: errors.New(`request header "Proxy-Connection" is not valid in HTTP/2`),
		},
		{
			h:    Header{"Keep-Alive": {""}},
			want: errors.New(`request header "Keep-Alive" is not valid in HTTP/2`),
		},
		{
			h:    Header{"Upgrade": {""}},
			want: errors.New(`request header "Upgrade" is not valid in HTTP/2`),
		},
	}
	for i, tt := range tests {
		got := checkValidHTTP2RequestHeaders(tt.h)
		if !equalError(got, tt.want) {
			t.Errorf("%d. checkValidHTTP2Request = %v; want %v", i, got, tt.want)
		}
	}
}

// TestCanonicalHeaderCacheGrowth verifies that the canonical header cache
// size is capped to a reasonable level.
func TestCanonicalHeaderCacheGrowth(t *testing.T) {
	for _, size := range []int{1, (1 << 20) - 10} {
		base := strings.Repeat("X", size)
		sc := &serverConn{
			serveG: newGoroutineLock(),
		}
		count := 0
		added := 0
		for added < 10*maxCachedCanonicalHeadersKeysSize {
			h := fmt.Sprintf("%v-%v", base, count)
			c := sc.canonicalHeader(h)
			if len(h) != len(c) {
				t.Errorf("sc.canonicalHeader(%q) = %q, want same length", h, c)
			}
			count++
			added += len(h)
		}
		total := 0
		for k, v := range sc.canonHeader {
			total += len(k) + len(v) + 100
		}
		if total > maxCachedCanonicalHeadersKeysSize {
			t.Errorf("after adding %v ~%v-byte headers, canonHeader cache is ~%v bytes, want <%v", count, size, total, maxCachedCanonicalHeadersKeysSize)
		}
	}
}

func TestFingerprintCollectorPriorityLimit(t *testing.T) {
	var collector fingerprintCollector
	for range maxFingerprintPriorities + 2 {
		collector.capturePriority(1, PriorityParam{})
	}
	if got, want := len(collector.priorities), maxFingerprintPriorities+1; got != want {
		t.Fatalf("captured priorities = %d; want %d", got, want)
	}
	if err := collector.fingerprint(nil, nil).Validate(); err == nil || !strings.Contains(err.Error(), "too many priorities") {
		t.Fatalf("Validate error = %v; want too many priorities", err)
	}
}

func TestResponseWriterRejectsInvalidExactHeaderBlocks(t *testing.T) {
	tests := []struct {
		name  string
		block HeaderBlock
	}{
		{
			name:  "empty",
			block: HeaderBlock{Kind: HeaderBlockInitial},
		},
		{
			name: "informational status in final block",
			block: HeaderBlock{
				Kind:   HeaderBlockInitial,
				Fields: []HeaderField{{Name: ":status", Value: "103"}},
			},
		},
		{
			name: "empty informational block",
			block: HeaderBlock{
				Kind: HeaderBlockInformational,
			},
		},
		{
			name: "final status in informational block",
			block: HeaderBlock{
				Kind:   HeaderBlockInformational,
				Fields: []HeaderField{{Name: ":status", Value: "200"}},
			},
		},
		{
			name: "pseudo after regular field",
			block: HeaderBlock{
				Kind: HeaderBlockInitial,
				Fields: []HeaderField{
					{Name: "x-regular", Value: "v"},
					{Name: ":status", Value: "200"},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rws := new(responseWriterState)
			w := responseWriter{rws: rws}
			if err := w.WriteHeaderBlock(tt.block); err == nil {
				t.Fatalf("WriteHeaderBlock(%#v) succeeded", tt.block)
			}
			if rws.headerStarted || rws.wroteHeader || rws.exactHeaderFields != nil {
				t.Fatal("invalid block mutated writer state")
			}
		})
	}
}

func TestResponseWriterRejectsInvalidExactTrailerBlock(t *testing.T) {
	rws := new(responseWriterState)
	w := responseWriter{rws: rws}
	block := HeaderBlock{
		Kind:   HeaderBlockTrailer,
		Fields: []HeaderField{{Name: ":status", Value: "200"}},
	}
	if err := w.SetTrailerBlock(block); err == nil {
		t.Fatal("SetTrailerBlock accepted a pseudo-header")
	}
	if rws.exactTrailerFields != nil {
		t.Fatal("invalid trailer block mutated writer state")
	}
}
