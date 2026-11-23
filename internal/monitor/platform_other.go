//go:build !darwin

package monitor

// Supported reports whether the monitor can run on this OS.
func Supported() bool { return false }

// NativePlatform returns adapters that refuse with ErrUnsupported, so every
// other pgbot command keeps building and working on Linux and Windows.
func NativePlatform(Paths) Platform {
	return Platform{Secrets: unsupported{}, Agents: unsupported{}, Notifier: unsupported{}}
}

type unsupported struct{}

func (unsupported) Set(string, string, string) error     { return ErrUnsupported }
func (unsupported) Get(string, string) (string, error)   { return "", ErrUnsupported }
func (unsupported) Delete(string, string) error          { return ErrUnsupported }
func (unsupported) Install(string, string, []byte) error { return ErrUnsupported }
func (unsupported) Remove(string, string) error          { return ErrUnsupported }
func (unsupported) Loaded(string) bool                   { return false }
func (unsupported) Kick(string) error                    { return ErrUnsupported }
func (unsupported) Notify(Notification, bool) error      { return ErrUnsupported }

// RemoveNotifierApp is a no-op off macOS.
func RemoveNotifierApp(string) error { return nil }
