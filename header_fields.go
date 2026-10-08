// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http

import (
	"io"

	"github.com/josexy/xhttp/httptrace"
)

// writeHTTP1Fields writes an already prepared field sequence. Validation,
// normalization and ordering belong to the caller; exact blocks must not pass
// through name grouping or acquire automatically generated fields here.
func writeHTTP1Fields(w io.Writer, fields []HeaderField, trace *httptrace.ClientTrace) error {
	ws, ok := w.(io.StringWriter)
	if !ok {
		ws = stringWriter{w}
	}
	for _, field := range fields {
		for _, part := range []string{field.Name, ": ", field.Value, "\r\n"} {
			if _, err := ws.WriteString(part); err != nil {
				return err
			}
		}
		if trace != nil && trace.WroteHeaderField != nil {
			trace.WroteHeaderField(field.Name, []string{field.Value})
		}
	}
	return nil
}
