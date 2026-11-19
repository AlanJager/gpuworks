package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/user"
	"time"

	"gpuworks.ai/gpuworks/internal/server/extpkg/layout"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pluginbridge"
	"gpuworks.ai/gpuworks/internal/server/pushstate"
)

// The offline maintenance commands, `plugin clock reset` and `plugin pstate
// clear`, run as root with the server stopped. Each checks its request
// against the database and appends one entry to the maintenance journal
// (pluginbridge.MaintenanceEntry); it changes nothing else. The next boot
// applies the journal through the paths a running server uses, which audit
// it, show its banner and let the plugins see it.

// effectiveUID is the caller's effective uid; tests replace it.
var effectiveUID = os.Geteuid

// maintenanceNow is the host's wall clock; tests replace it.
var maintenanceNow = time.Now

// requireRoot refuses a maintenance command run by anyone but root: what it
// records overrides what the plugins decided.
func requireRoot(name string) error {
	if effectiveUID() != 0 {
		return fmt.Errorf("gpuworks plugin %s must run as root (sudo)", name)
	}
	return nil
}

// maintenanceBy names who ran a maintenance command: the account that
// invoked sudo, otherwise the account itself.
func maintenanceBy() string {
	if u := os.Getenv("SUDO_USER"); u != "" && u != "root" {
		return u
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "root"
}

// recordMaintenance appends e to the data directory's maintenance journal
// and returns the journal's path. A journal or plugins directory root
// creates gets the data directory's owner, so the server's own account can
// apply and empty it.
func recordMaintenance(dataDir string, e pluginbridge.MaintenanceEntry) (string, error) {
	dir := layout.New(dataDir).Root
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	ownPathLikeDataDir(dir, dataDir)
	path := pluginbridge.MaintenancePath(dir)
	if err := pluginbridge.AppendMaintenance(path, e); err != nil {
		return "", fmt.Errorf("record in the maintenance journal %s: %w", path, err)
	}
	ownPathLikeDataDir(path, dataDir)
	return path, nil
}

func ownPathLikeDataDir(path, dataDir string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	ownLikeDataDir(f, dataDir)
}

// pluginClock runs `plugin clock reset --to TIME --reason TEXT`: it records
// a correction of the plugin clock (extclock) that a clock which ran ahead
// carried forward. The stored high-water mark must be ahead of the wall
// clock, and the target lies between the wall clock and that mark, at most
// pushstate.MaxClockCorrection before the mark.
func (o Options) pluginClock(log *slog.Logger, args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "reset" {
		fmt.Fprintln(stdout, "usage:\n"+pluginUsage)
		return flag.ErrHelp
	}
	fs, p := pluginFlags("clock reset")
	to := fs.String("to", "", "the correct time, RFC 3339 (2027-01-15T08:00:00Z), not before the wall clock")
	reason := fs.String("reason", "", "why the clock is reset, for the audit")
	rest, err := parseInterleaved(fs, args[1:])
	if err != nil {
		return err
	}
	if len(rest) != 0 || *to == "" || *reason == "" {
		return errors.New("plugin clock reset takes --to TIME and --reason TEXT and nothing else")
	}
	target, err := time.Parse(time.RFC3339, *to)
	if err != nil {
		return fmt.Errorf("plugin clock reset: --to %q is not an RFC 3339 time", *to)
	}
	if err := requireRoot("clock reset"); err != nil {
		return err
	}
	_, st, done, err := o.openStoppedHost(p)
	if err != nil {
		return err
	}
	defer done()
	snap, err := st.LoadPushedState(context.Background())
	if err != nil {
		return err
	}
	wall := maintenanceNow().UTC()
	if snap.Clock == nil || snap.Clock.HWMMS <= wall.UnixMilli() {
		return fmt.Errorf("the plugin clock is not ahead of the wall clock (%s); there is nothing to reset", wall.Format(time.RFC3339))
	}
	hwm := time.UnixMilli(snap.Clock.HWMMS).UTC()
	switch {
	case target.Before(wall):
		return fmt.Errorf("--to %s is before the wall clock %s; the plugin clock never runs behind it, so set the wall clock first", target.Format(time.RFC3339), wall.Format(time.RFC3339))
	case target.After(hwm):
		return fmt.Errorf("--to %s is after the plugin clock %s; a reset only moves it back", target.Format(time.RFC3339), hwm.Format(time.RFC3339))
	case hwm.Sub(target) > pushstate.MaxClockCorrection:
		days := int(pushstate.MaxClockCorrection / (24 * time.Hour))
		return messages.NewProductError(messages.ErrorPluginClockCorrectionTooLarge,
			fmt.Sprintf("--to %s is more than %d days before the plugin clock %s", target.Format(time.RFC3339), days, hwm.Format(time.RFC3339)),
			map[string]any{"max_days": days})
	}
	e := pluginbridge.MaintenanceEntry{V: pluginbridge.MaintenanceVersion, Op: pluginbridge.MaintenanceClockReset,
		At: wall, By: maintenanceBy(), To: target.UTC(), Reason: *reason}
	path, err := recordMaintenance(p.data, e)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "recorded in %s: at its next start the server moves the plugin clock from %s back to %s\n",
		path, hwm.Format(time.RFC3339), e.To.Format(time.RFC3339))
	log.Info("plugin clock reset recorded", "by", e.By, "from", hwm.Format(time.RFC3339), "to", e.To.Format(time.RFC3339), "reason", e.Reason, "data", p.data)
	return nil
}
