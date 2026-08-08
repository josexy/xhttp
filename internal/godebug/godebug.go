// Copyright 2026 The xhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package godebug provides the subset of internal/godebug used by xhttp.
package godebug

import (
	"os"
	"strings"
	"sync/atomic"
)

// A Setting is a single setting in the GODEBUG environment variable.
type Setting struct {
	name       string
	nonDefault atomic.Uint64
}

// New returns a new Setting for name.
func New(name string) *Setting {
	return &Setting{name: name}
}

// Name returns the setting name without the marker used for undocumented
// settings.
func (s *Setting) Name() string {
	if s.name != "" && s.name[0] == '#' {
		return s.name[1:]
	}
	return s.name
}

// Undocumented reports whether the setting was created with a leading '#'.
func (s *Setting) Undocumented() bool {
	return s.name != "" && s.name[0] == '#'
}

// String returns name=value for the current setting.
func (s *Setting) String() string {
	return s.Name() + "=" + s.Value()
}

// Value returns the last value assigned to this setting in GODEBUG.
//
// The standard library's internal implementation can conditionally enable a
// value for matching call stacks using a #pattern suffix. That matcher is not
// available outside GOROOT, so this compatibility implementation uses the
// value before the suffix unconditionally.
func (s *Setting) Value() string {
	name := s.Name()
	value := ""
	found := false
	for entry := range strings.SplitSeq(os.Getenv("GODEBUG"), ",") {
		key, candidate, ok := strings.Cut(entry, "=")
		if ok && key == name {
			value = candidate
			found = true
		}
	}
	if !found {
		return ""
	}
	if value, _, ok := strings.Cut(value, "#"); ok {
		return value
	}
	return value
}

// IncNonDefault records that non-default behavior was used. The count is kept
// locally because external packages cannot register runtime GODEBUG metrics.
func (s *Setting) IncNonDefault() {
	s.nonDefault.Add(1)
}
