package app

import (
	"log/slog"
	"os"
)

// envTestHooks turns on window.__excaTest (internal/launch's
// WithTestHooks) on every /launch through this instance, independent of
// --fake-host: the dockerized-Drive smoke suite launches through the
// real Drive flow, which never enables fake-host mode, so it needs its
// own knob for scene introspection. Read directly with os.Getenv, not
// through internal/config, for the same reason envFakeHost is: a dev/e2e-only
// flag needs no validation or documented default that would earn it a
// place there.
const envTestHooks = "EXCALIDRAW_WOPI_TEST_HOOKS"

// testHooksAllowed reports whether NewServer should set testHooks: true
// in every launch config. Unlike fakeHostAllowed, this carries no
// loopback restriction: window.__excaTest is a read-only accessor that
// discloses nothing a page's own script could not already read straight
// off the live Excalidraw API (see useExcalidrawStore.ts), so exposing it
// is not itself an auth bypass the way the fake host's unauthenticated
// WOPI routes are.
func testHooksAllowed() bool {
	return os.Getenv(envTestHooks) == "1"
}

// logTestHooksWarning logs once, loudly, that test-hook mode is running.
func logTestHooksWarning() {
	slog.Warn(envTestHooks + "=1: every /launch sets testHooks: true, exposing window.__excaTest client-side; " +
		"e2e suite use only, never enable this in production")
}
