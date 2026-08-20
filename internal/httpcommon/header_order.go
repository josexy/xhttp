// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package httpcommon

import (
	"sort"

	"github.com/josexy/net/http2/hpack"
)

// OrderHeaderFields groups fields by name and returns them in order. Listed
// pseudo-header fields are emitted first, followed by unlisted pseudo-header
// fields in their existing order. Listed regular fields follow, and remaining
// regular fields are emitted in lowercase lexical order. Values for one field
// name retain their existing order.
//
// Names in order must already be lowercase and validated by the caller.
func OrderHeaderFields(fields []hpack.HeaderField, order []string) []hpack.HeaderField {
	if len(order) == 0 || len(fields) == 0 {
		return fields
	}

	groups := make(map[string][]hpack.HeaderField, len(fields))
	pseudoNames := make([]string, 0, 5)
	regularNames := make([]string, 0, len(fields))
	for _, f := range fields {
		if _, ok := groups[f.Name]; !ok {
			if len(f.Name) > 0 && f.Name[0] == ':' {
				pseudoNames = append(pseudoNames, f.Name)
			} else {
				regularNames = append(regularNames, f.Name)
			}
		}
		groups[f.Name] = append(groups[f.Name], f)
	}

	ordered := make([]hpack.HeaderField, 0, len(fields))
	emitted := make(map[string]bool, len(groups))
	emit := func(name string) {
		if emitted[name] {
			return
		}
		if group := groups[name]; len(group) > 0 {
			ordered = append(ordered, group...)
			emitted[name] = true
		}
	}

	for _, name := range order {
		if len(name) > 0 && name[0] == ':' {
			emit(name)
		}
	}
	for _, name := range pseudoNames {
		emit(name)
	}
	for _, name := range order {
		if len(name) == 0 || name[0] != ':' {
			emit(name)
		}
	}
	sort.Strings(regularNames)
	for _, name := range regularNames {
		emit(name)
	}
	return ordered
}
