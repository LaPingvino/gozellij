package fabric

import "errors"

// ErrOutputClosed is returned by writes to or attaches on a closed OutputBuffer. Reported rather
// than ignored: it means the caller is holding a stale handle, which is a bug worth seeing.
var ErrOutputClosed = errors.New("output buffer is closed")
