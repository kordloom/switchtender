package dispatch

import (
	"context"

	"github.com/dcadolph/switchtender/internal/run"
)

// Notifier delivers a terminal top-level run to an external channel. Register one with
// RegisterNotifier so a new channel plugs in beside the built-in webhook, Slack, and email
// delivery without editing the dispatcher.
type Notifier interface {
	// Notify delivers the run to the channel. The run is already redacted of extra vars, so a
	// notifier never receives survey answers or template vars that can carry secrets.
	Notify(ctx context.Context, r *run.Run) error
}

// NotifierFunc adapts a plain function to a Notifier.
type NotifierFunc func(ctx context.Context, r *run.Run) error

// Notify calls the underlying function.
func (f NotifierFunc) Notify(ctx context.Context, r *run.Run) error {
	return f(ctx, r)
}

// notifiers holds channels added by an extension, keyed by name. Registration happens at startup,
// before runs finish, so reads during delivery need no lock, matching secretsource.
var notifiers = map[string]Notifier{}

// RegisterNotifier adds a notification channel under name. It panics on an empty or duplicate name
// or a nil notifier, which is a programming error caught at startup. The names discord, ntfy, and
// teams are claimed by the official switchtender-plugins binary, so a future built-in must not take
// them.
func RegisterNotifier(name string, n Notifier) {
	if name == "" {
		panic("dispatch: cannot register an empty notifier name")
	}
	if n == nil {
		panic("dispatch: nil notifier for " + name)
	}
	if _, exists := notifiers[name]; exists {
		panic("dispatch: duplicate notifier " + name)
	}
	notifiers[name] = n
}
