// Copyright 2026 The xhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !race

// Package race exposes whether the race detector is enabled to relocated
// upstream tests.
package race

// Enabled reports whether this binary was built with -race.
const Enabled = false
