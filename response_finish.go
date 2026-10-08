package http

import "errors"

// FinishResponse synchronously finishes an HTTP/2 response, including pending
// DATA, trailers and END_STREAM, and reports write failure. The handler must not
// write any more response data after calling it. Normal handler cleanup remains
// automatic; calling FinishResponse again returns the original result. Like
// other ResponseWriter methods, it must not be used after the handler returns.
//
// It follows Unwrap methods through response writer wrappers. HTTP/1 writers
// return errors.ErrUnsupported (their normal connection writer owns completion).
// This observes successful writes to the connection, not peer acknowledgment.
func FinishResponse(w ResponseWriter) error {
	for range 100 {
		if finisher, ok := w.(interface{ FinishResponse() error }); ok {
			return finisher.FinishResponse()
		}
		unwrapper, ok := w.(interface{ Unwrap() ResponseWriter })
		if !ok {
			return errors.ErrUnsupported
		}
		w = unwrapper.Unwrap()
		if w == nil {
			return errors.ErrUnsupported
		}
	}
	return errors.ErrUnsupported
}
