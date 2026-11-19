package pluginbridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pushstate"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// The offline maintenance journal: root runs `gpuworks plugin clock reset`
// and `gpuworks plugin pstate clear` with the server stopped, and each
// appends one MaintenanceEntry to <data>/plugins/maintenance.jsonl. The
// commands change nothing else. At its next boot the server applies the
// entries in order through the same paths a running server uses
// (pushstate.State.CorrectClock and ClearTable), so each is audited, shows
// its banner and reaches the plugins, and a running server never undoes
// one (docs/dev/extensions.md, ADR 0071 decision 9).

// MaintenanceFile is the journal's name in the plugins directory.
const MaintenanceFile = "maintenance.jsonl"

// MaintenanceVersion is the version of MaintenanceEntry this build writes
// and applies.
const MaintenanceVersion = 1

// Maintenance operations.
const (
	// MaintenanceClockReset moves extclock back to To.
	MaintenanceClockReset = "clock_reset"
	// MaintenancePstateClear clears Owner's restriction table of Kind.
	MaintenancePstateClear = "pstate_clear"
)

// maintenanceApplyWait bounds how long boot waits for one entry's commit.
const maintenanceApplyWait = 30 * time.Second

// MaintenanceEntry is one line of the journal.
type MaintenanceEntry struct {
	// V is MaintenanceVersion.
	V int `json:"v"`
	// Op is MaintenanceClockReset or MaintenancePstateClear.
	Op string `json:"op"`
	// At is when the command ran, by the host's wall clock.
	At time.Time `json:"at"`
	// By is who ran it: the account that invoked sudo, or the root account.
	By string `json:"by"`
	// Owner and Kind name the table a clear removes.
	Owner string `json:"owner,omitempty"`
	Kind  string `json:"kind,omitempty"`
	// To is the instant a clock reset moves extclock to.
	To time.Time `json:"to,omitzero"`
	// Reason is what the operator gave, for the audit.
	Reason string `json:"reason"`
}

// Check reports whether e is an entry this build can apply. A refusal is a
// *messages.ProductError (BAD_REQUEST).
func (e MaintenanceEntry) Check() error {
	bad := func(format string, a ...any) error {
		return messages.NewProductError(messages.ErrorBadRequest, "maintenance entry: "+fmt.Sprintf(format, a...), nil)
	}
	switch {
	case e.V != MaintenanceVersion:
		return bad("version %d is not %d", e.V, MaintenanceVersion)
	case e.By == "":
		return bad("names nobody who ran it")
	case e.Reason == "":
		return bad("gives no reason")
	}
	switch e.Op {
	case MaintenanceClockReset:
		if e.To.IsZero() || e.Owner != "" || e.Kind != "" {
			return bad("a clock reset takes a time and nothing else")
		}
	case MaintenancePstateClear:
		if !extapi.ValidPackageID(e.Owner) || !e.To.IsZero() {
			return bad("a clear takes a package id and a kind")
		}
		if extapi.ClassOf(e.Kind) != extapi.Restrict {
			return pushstate.NotARestriction(e.Owner, e.Kind)
		}
	default:
		return bad("operation %q is unknown", e.Op)
	}
	return nil
}

// MaintenancePath is the journal in pluginsDir, the data directory's
// plugins directory.
func MaintenancePath(pluginsDir string) string { return filepath.Join(pluginsDir, MaintenanceFile) }

// AppendMaintenance appends e to the journal at path, creating it with mode
// 0600, and syncs the file and its directory, so an entry the command
// reported is there at the next boot. The caller holds the server lock: no
// server runs meanwhile.
func AppendMaintenance(path string, e MaintenanceEntry) error {
	if err := e.Check(); err != nil {
		return err
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// journalLine is one line of the journal as read: its bytes, kept as they
// are for a rewrite, and the entry or why it is not one.
type journalLine struct {
	raw   []byte
	entry MaintenanceEntry
	err   error
}

// ReadMaintenance reads the journal at path; a missing journal has no
// entries. A line that is not an entry this build applies is returned
// with its error.
func ReadMaintenance(path string) ([]MaintenanceEntry, []error, error) {
	lines, err := readJournal(path)
	if err != nil {
		return nil, nil, err
	}
	entries, errs := make([]MaintenanceEntry, len(lines)), make([]error, len(lines))
	for i, l := range lines {
		entries[i], errs[i] = l.entry, l.err
	}
	return entries, errs, nil
}

func readJournal(path string) ([]journalLine, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []journalLine
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		text := bytes.TrimSpace(sc.Bytes())
		if len(text) == 0 {
			continue
		}
		l := journalLine{raw: append([]byte(nil), text...)}
		dec := json.NewDecoder(bytes.NewReader(text))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&l.entry); err != nil {
			l.err = messages.NewProductError(messages.ErrorBadRequest, "maintenance entry does not decode: "+err.Error(), nil)
		} else {
			l.err = l.entry.Check()
		}
		out = append(out, l)
	}
	return out, sc.Err()
}

// applyMaintenance applies the journal's entries in order, before any
// plugin starts: a clock reset through State.CorrectClock (whose effect
// writes the audit line and whose banner names who reset it), a clear
// through State.ClearTable. Each applied entry is logged with audit=true,
// by and reason. The journal is emptied once every entry applied. The
// first entry that fails stops it: that entry and the ones after it stay
// in the journal for the next boot, and the administrator banner
// PLUGIN_MAINTENANCE_FAILED{op, code} is shown until then.
func (b *Bridge) applyMaintenance(ctx context.Context) {
	path := MaintenancePath(b.pkgs.Dir.Root)
	lines, err := readJournal(path)
	if err != nil {
		b.log.Error("offline maintenance journal cannot be read; nothing in it is applied", "path", path, "err", err)
		b.failMaintenance("journal", failureCode(err))
		return
	}
	if len(lines) == 0 {
		return
	}
	for i, l := range lines {
		err := l.err
		if err == nil {
			err = b.applyEntry(ctx, l.entry)
		}
		if err != nil {
			op := l.entry.Op
			if op == "" {
				op = "unknown"
			}
			b.log.Error("offline maintenance not applied; it stays in the journal for the next boot", "op", op, "code", failureCode(err),
				"by", l.entry.By, "reason", l.entry.Reason, "err", err)
			b.failMaintenance(op, failureCode(err))
			if werr := rewriteJournal(path, lines[i:]); werr != nil {
				b.log.Error("offline maintenance journal cannot be rewritten; the entries before the failed one apply again at the next boot", "path", path, "err", werr)
			}
			return
		}
		e := l.entry
		b.log.Info("offline maintenance applied", "audit", true, "op", e.Op, "by", e.By, "reason", e.Reason,
			"at", e.At.UTC().Format(time.RFC3339), "plugin", e.Owner, "kind", e.Kind, "to", formatTo(e.To))
	}
	if err := rewriteJournal(path, nil); err != nil {
		b.log.Error("offline maintenance journal cannot be emptied; its entries apply again at the next boot", "path", path, "err", err)
	}
}

func formatTo(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func (b *Bridge) applyEntry(ctx context.Context, e MaintenanceEntry) error {
	ctx, cancel := context.WithTimeout(ctx, maintenanceApplyWait)
	defer cancel()
	switch e.Op {
	case MaintenanceClockReset:
		_, _, err := b.state.CorrectClock(ctx, e.To, e.By, e.Reason)
		return err
	case MaintenancePstateClear:
		return b.state.ClearTable(ctx, e.Owner, e.Kind)
	}
	return e.Check()
}

// failMaintenance shows the banner of a journal entry that did not apply.
func (b *Bridge) failMaintenance(op, code string) {
	b.maintenance.Store(&pushstate.Banner{ID: "plugins.maintenance_failed", Severity: extapi.SeverityWarning,
		Notice: messages.NewProductError(messages.ErrorPluginMaintenanceFailed, "offline maintenance "+op+" could not be applied: "+code,
			map[string]any{"op": op, "code": code})})
	b.refreshHealth()
}

// failureCode is err's product code; a failure without one (the database, a
// timeout) is INTERNAL_ERROR.
func failureCode(err error) string {
	var pe *messages.ProductError
	if errors.As(err, &pe) {
		return pe.Code()
	}
	if code := extapi.CodeOf(err); code != "" {
		return code
	}
	return messages.ErrorInternal
}

// rewriteJournal replaces the journal with lines, through a synced
// temporary file renamed over it.
func rewriteJournal(path string, lines []journalLine) error {
	var buf bytes.Buffer
	for _, l := range lines {
		buf.Write(l.raw)
		buf.WriteByte('\n')
	}
	tmp := path + ".partial"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return err
	}
	return nil
}
