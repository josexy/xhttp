// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package httpcommon

import (
	"reflect"
	"testing"

	"golang.org/x/net/http2/hpack"
)

func TestOrderHeaderFields(t *testing.T) {
	fields := []hpack.HeaderField{
		{Name: ":authority", Value: "example.com"},
		{Name: ":method", Value: "GET"},
		{Name: ":path", Value: "/"},
		{Name: ":scheme", Value: "https"},
		{Name: "z-last", Value: "z"},
		{Name: "x-repeat", Value: "one"},
		{Name: "a-first", Value: "a"},
		{Name: "x-repeat", Value: "two", Sensitive: true},
	}
	order := []string{":method", ":authority", "x-repeat"}
	want := []hpack.HeaderField{
		{Name: ":method", Value: "GET"},
		{Name: ":authority", Value: "example.com"},
		{Name: ":path", Value: "/"},
		{Name: ":scheme", Value: "https"},
		{Name: "x-repeat", Value: "one"},
		{Name: "x-repeat", Value: "two", Sensitive: true},
		{Name: "a-first", Value: "a"},
		{Name: "z-last", Value: "z"},
	}
	if got := OrderHeaderFields(fields, order); !reflect.DeepEqual(got, want) {
		t.Fatalf("OrderHeaderFields() = %#v; want %#v", got, want)
	}
}

func TestOrderHeaderFieldsNoOrderReturnsInput(t *testing.T) {
	fields := []hpack.HeaderField{{Name: "z", Value: "1"}, {Name: "a", Value: "2"}}
	got := OrderHeaderFields(fields, nil)
	if &got[0] != &fields[0] {
		t.Fatal("OrderHeaderFields with no order copied its input")
	}
	if !reflect.DeepEqual(got, fields) {
		t.Fatalf("OrderHeaderFields() = %#v; want %#v", got, fields)
	}
}
