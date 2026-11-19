package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"

	"gpuworks.ai/gpuworks/internal/server/pluginbridge"
	"gpuworks.ai/gpuworks/internal/server/pushstate"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// pluginPstate runs `plugin pstate clear ID KIND --reason TEXT`: it records
// the release of one restriction table a plugin pushed (fleet_capacity or
// node_holds) together with the administrator's overrides of it, for a
// plugin that cannot release it itself. Grants are never cleared.
func (o Options) pluginPstate(log *slog.Logger, args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "clear" {
		fmt.Fprintln(stdout, "usage:\n"+pluginUsage)
		return flag.ErrHelp
	}
	fs, p := pluginFlags("pstate clear")
	reason := fs.String("reason", "", "why the table is cleared, for the audit")
	pos, err := parseInterleaved(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 2 || *reason == "" {
		return errors.New("plugin pstate clear takes a package id, a table kind and --reason TEXT")
	}
	id, kind := pos[0], pos[1]
	if !extapi.ValidPackageID(id) {
		return fmt.Errorf("plugin pstate clear: %q is not a package id", id)
	}
	if extapi.ClassOf(kind) != extapi.Restrict {
		return pushstate.NotARestriction(id, kind)
	}
	if err := requireRoot("pstate clear"); err != nil {
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
	own := ownerState(snap, id)
	held, overrides := false, 0
	for _, r := range own.Owners {
		held = held || r.Kind == kind
	}
	for _, ov := range own.Overrides {
		if ov.Kind == kind {
			overrides++
		}
	}
	e := pluginbridge.MaintenanceEntry{V: pluginbridge.MaintenanceVersion, Op: pluginbridge.MaintenancePstateClear,
		At: maintenanceNow().UTC(), By: maintenanceBy(), Owner: id, Kind: kind, Reason: *reason}
	path, err := recordMaintenance(p.data, e)
	if err != nil {
		return err
	}
	if held {
		fmt.Fprintf(stdout, "recorded in %s: at its next start the server clears %s of %s (%s) and %d overrides of it\n",
			path, kind, id, heldRows(own, kind), overrides)
	} else {
		fmt.Fprintf(stdout, "recorded in %s: %s holds no %s table now; clearing it at the next start changes nothing\n", path, id, kind)
	}
	log.Info("plugin pushed state clear recorded", "by", e.By, "plugin", id, "kind", kind, "held", held, "overrides", overrides, "reason", e.Reason, "data", p.data)
	return nil
}

// heldRows describes the rows of own's table of kind.
func heldRows(own ownedState, kind string) string {
	switch kind {
	case extapi.KindNodeHolds:
		return fmt.Sprintf("%d node holds", len(own.Rows.NodeHolds))
	case extapi.KindFleetCapacity:
		return fmt.Sprintf("%d capacity steps", len(own.Rows.CapacitySteps))
	}
	return "its rows"
}
