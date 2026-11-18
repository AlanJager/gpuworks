package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"text/tabwriter"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store/pstate"
)

// ownedState is the part of the pushed state one owner wrote.
type ownedState struct {
	Owners    []pstate.Owner
	Rows      pstate.Rows
	Down      []pstate.Down
	Overrides []pstate.Override
}

func ownedBy[T any](rows []T, owner string, of func(*T) string) []T {
	var out []T
	for i := range rows {
		if of(&rows[i]) == owner {
			out = append(out, rows[i])
		}
	}
	return out
}

// ownerState filters snap to owner's rows. An offline command reads the
// whole snapshot once; the tables are bounded by the kinds' row limits.
func ownerState(snap pstate.Snapshot, owner string) ownedState {
	r := &snap.Rows
	return ownedState{
		Owners: ownedBy(snap.Owners, owner, func(x *pstate.Owner) string { return x.Owner }),
		Rows: pstate.Rows{
			Availability:       ownedBy(r.Availability, owner, func(x *pstate.AvailabilityTable) string { return x.Owner }),
			AvailabilityRows:   ownedBy(r.AvailabilityRows, owner, func(x *pstate.AvailabilityRow) string { return x.Owner }),
			AvailabilitySteps:  ownedBy(r.AvailabilitySteps, owner, func(x *pstate.AvailabilityStep) string { return x.Owner }),
			AvailabilityLimits: ownedBy(r.AvailabilityLimits, owner, func(x *pstate.AvailabilityLimit) string { return x.Owner }),
			FleetCapacity:      ownedBy(r.FleetCapacity, owner, func(x *pstate.FleetCapacity) string { return x.Owner }),
			CapacitySteps:      ownedBy(r.CapacitySteps, owner, func(x *pstate.CapacityStep) string { return x.Owner }),
			CapacityKeep:       ownedBy(r.CapacityKeep, owner, func(x *pstate.CapacityKeep) string { return x.Owner }),
			NodeHolds:          ownedBy(r.NodeHolds, owner, func(x *pstate.NodeHold) string { return x.Owner }),
			Banners:            ownedBy(r.Banners, owner, func(x *pstate.Banner) string { return x.Owner }),
			BannerSteps:        ownedBy(r.BannerSteps, owner, func(x *pstate.BannerStep) string { return x.Owner }),
			Attestations:       ownedBy(r.Attestations, owner, func(x *pstate.Attestation) string { return x.Owner }),
			Params:             ownedBy(r.Params, owner, func(x *pstate.NoticeParam) string { return x.Owner }),
		},
		Down:      ownedBy(snap.Down, owner, func(x *pstate.Down) string { return x.Owner }),
		Overrides: ownedBy(snap.Overrides, owner, func(x *pstate.Override) string { return x.Owner }),
	}
}

// empty reports whether the owner has no row left anywhere.
func (s ownedState) empty() bool {
	r := s.Rows
	return len(s.Owners)+len(s.Down)+len(s.Overrides)+len(r.Availability)+len(r.AvailabilityRows)+
		len(r.AvailabilitySteps)+len(r.AvailabilityLimits)+len(r.FleetCapacity)+len(r.CapacitySteps)+
		len(r.CapacityKeep)+len(r.NodeHolds)+len(r.Banners)+len(r.BannerSteps)+len(r.Attestations)+len(r.Params) == 0
}

// pluginState prints what a package pushed, from the database. It writes
// nothing, so it runs beside a live server, whose writer commits at most
// one tick later than what it holds.
func (o Options) pluginState(args []string, stdout io.Writer) error {
	fs, p := pluginFlags("state")
	id, err := onePackageID(fs, "state", args)
	if err != nil {
		return err
	}
	_, st, err := o.openHost(p)
	if err != nil {
		return err
	}
	defer st.Close()
	snap, err := st.LoadPushedState(context.Background())
	if err != nil {
		return err
	}
	if held, pid := serverRunning(p.data); pid > 0 {
		fmt.Fprintf(stdout, "the server (pid %d) is running: these rows may be up to one tick behind it\n", pid)
	} else if held {
		fmt.Fprintf(stdout, "another gpuworks process holds %s: if it is the server, these rows may be up to one tick behind it\n", filepath.Join(p.data, serverLockFile))
	}
	own := ownerState(snap, id)
	if own.empty() {
		fmt.Fprintf(stdout, "%s has no pushed state\n", id)
		return nil
	}
	return printOwnedState(stdout, own)
}

func stamp(ms int64) string { return time.UnixMilli(ms).UTC().Format(time.RFC3339) }

func optInt(v *int64) string {
	if v == nil {
		return "-"
	}
	return strconv.FormatInt(*v, 10)
}

func printOwnedState(w io.Writer, s ownedState) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TABLE\tSCHEMA\tGENERATION\tPUSHED\tDIGEST")
	for _, o := range s.Owners {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%s\n", o.Kind, o.Schema, o.Generation, stamp(o.PushedAtMS), o.Digest)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	r := s.Rows
	for _, a := range r.Availability {
		fmt.Fprintf(w, "availability: down grace %ds\n", a.DownGraceS)
	}
	for _, a := range r.AvailabilityRows {
		fmt.Fprintf(w, "availability %s: down access %s\n", a.Feature, a.DownAccess)
	}
	for _, a := range r.AvailabilitySteps {
		fmt.Fprintf(w, "availability %s step %d at %s: access %s notice %s\n", a.Feature, a.Step, stamp(a.AtMS), a.Access, a.NoticeCode)
	}
	for _, a := range r.AvailabilityLimits {
		max := strconv.FormatInt(a.Max, 10)
		if a.Unlimited {
			max = "unlimited"
		}
		fmt.Fprintf(w, "availability %s step %d limit %s: %s\n", a.Feature, a.Step, a.Dim, max)
	}
	for _, c := range r.FleetCapacity {
		fmt.Fprintf(w, "fleet_capacity: select %s\n", c.Select)
	}
	for _, c := range r.CapacitySteps {
		fmt.Fprintf(w, "fleet_capacity step %d at %s: max nodes with cards %s, max cards %s\n", c.Step, stamp(c.AtMS), optInt(c.MaxNodesWithCards), optInt(c.MaxCards))
	}
	for _, k := range r.CapacityKeep {
		fmt.Fprintf(w, "fleet_capacity keeps node %d\n", k.NodeID)
	}
	for _, h := range r.NodeHolds {
		fmt.Fprintf(w, "node_holds: node %d held (%s)\n", h.NodeID, h.Code)
	}
	for _, b := range r.Banners {
		fmt.Fprintf(w, "banner %s: audience %s\n", b.BannerID, b.Audience)
	}
	for _, b := range r.BannerSteps {
		fmt.Fprintf(w, "banner %s step %d at %s: severity %s hidden %t notice %s\n", b.BannerID, b.Step, stamp(b.AtMS), b.Severity, b.Hidden, b.NoticeCode)
	}
	for _, a := range r.Attestations {
		fmt.Fprintf(w, "attestation %s: feature %s format %s, %d bytes\n", a.Name, a.Feature, a.Format, len(a.Blob))
	}
	for _, n := range r.Params {
		fmt.Fprintf(w, "notice param %s %s %s %s: %s\n", n.Kind, n.Entry, n.Slot, n.Key, paramValue(n))
	}
	for _, d := range s.Down {
		fmt.Fprintf(w, "down since %s\n", stamp(d.SinceMS))
	}
	for _, o := range s.Overrides {
		fmt.Fprintf(w, "override %d: %s %s %s value %s by user %d at %s: %s\n", o.ID, o.Action, o.Kind, o.EntryKey, optInt(o.Value), o.ByUser, stamp(o.AtMS), o.Reason)
	}
	return nil
}

// paramValue is the one value a notice parameter carries.
func paramValue(n pstate.NoticeParam) string {
	switch {
	case n.Str != nil:
		return strconv.Quote(*n.Str)
	case n.Int != nil:
		return strconv.FormatInt(*n.Int, 10)
	case n.Bool != nil:
		return strconv.FormatBool(*n.Bool)
	}
	return "-"
}
