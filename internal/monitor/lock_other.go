//go:build !unix

package monitor

import "errors"

// ErrBusy means another check holds the lock.
var ErrBusy = errors.New("another PgBot Monitor check is already running")

// Lock is unavailable off Unix; the monitor itself is macOS-only.
func Lock(string) (func(), error) { return nil, ErrUnsupported }
