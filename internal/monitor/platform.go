package monitor

import "errors"

// ErrUnsupported is returned by every platform adapter off macOS.
var ErrUnsupported = errors.New("PgBot Monitor currently supports macOS only")

// ErrNoSecret means the Keychain holds no saved connection.
var ErrNoSecret = errors.New("no saved connection in the Keychain")

// Secrets stores the connection string. On macOS: the login Keychain.
type Secrets interface {
	Set(service, account, secret string) error
	Get(service, account string) (string, error)
	Delete(service, account string) error
}

// Agents installs and removes the scheduled LaunchAgent.
type Agents interface {
	// Install writes the plist and (re)loads it, replacing any previous job
	// with the same label — never leaving two.
	Install(label, plistPath string, plist []byte) error
	// Remove unloads the job and deletes its plist; absent is not an error.
	Remove(label, plistPath string) error
	// Loaded reports whether launchd currently has the job.
	Loaded(label string) bool
	// Kick starts the job now, as launchd would on schedule (for tests).
	Kick(label string) error
}

// Notifier posts a banner to Notification Center. A nil error means macOS
// ACCEPTED the request — not that the user saw it (permissions and Focus
// decide that, and neither is observable from here).
type Notifier interface {
	Notify(n Notification, sound bool) error
}

// Platform bundles the OS adapters so the runner and CLI can be tested with fakes.
type Platform struct {
	Secrets  Secrets
	Agents   Agents
	Notifier Notifier
}

// KeychainAccount is the account name the connection is stored under.
const KeychainAccount = "connection"
