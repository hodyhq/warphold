//go:build linux

package tray

import (
	"github.com/godbus/dbus/v5"
	"github.com/pkg/errors"
)

// notifyTimeout lets the server pick its own expiry (-1), so a failure
// notification follows the desktop's own policy for critical messages.
const notifyTimeout = int32(-1)

// notify raises a desktop notification over org.freedesktop.Notifications. It
// carries a summary and a body only: no path from engine.json, no local
// session URL, no credentials.
//
// The session bus connection is the shared one dbus keeps for the process, so
// it is not closed here.
func notify(summary, body string) error {
	conn, err := dbus.SessionBus()
	if err != nil {
		return errors.Wrap(err, "no session bus")
	}

	obj := conn.Object("org.freedesktop.Notifications", "/org/freedesktop/Notifications")

	call := obj.Call("org.freedesktop.Notifications.Notify", 0,
		"WarpHold",     // app_name
		uint32(0),      // replaces_id: 0, never replace an unrelated notification
		"dialog-error", // app_icon
		summary,
		body,
		[]string{}, // no actions: the menu is where the actions live
		map[string]dbus.Variant{"urgency": dbus.MakeVariant(byte(2))},
		notifyTimeout,
	)

	return errors.Wrap(call.Err, "notify")
}
