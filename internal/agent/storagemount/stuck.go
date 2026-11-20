package storagemount

import (
	"context"
	"path/filepath"
	"strings"
	"time"
)

// stuckSince is what the manager remembers of a storage that is unresponsive.
type stuckSince struct {
	since, forced time.Time
	path          string
	// eligible is set while the storage's own mount could be forced off as
	// far as it alone is concerned: one call blocked for the threshold, the
	// agent's own mount, nothing nested, not in use.
	eligible bool
}

// A filesystem call that timed out is left running and its key stays busy
// until it returns (guard). When the server answers again the call returns
// and the next reconcile finds the storage ready; nothing here is needed for
// that. A hard NFS mount can also keep the call blocked for good, and then
// every later check fails at once and the storage would stay unresponsive
// until someone unmounts it by hand or restarts the agent.
//
// recoverStuck is the way out of that for a mount this agent created. It
// acts only on a call that is still blocked: once one single call on the
// storage has been in flight for Options.StuckRemountAfter it forces the
// mount off ("umount -f -l", which makes the NFS client fail the calls in
// flight and detaches the mount point even while they are blocked), gives up
// the calls still blocked on it and mounts the export again in the same
// pass. A mount that answers every call, only slower than the time limit, is
// never unmounted. It is tried at most once per StuckRemountAfter and stops
// for good once Options.StuckMaxAbandoned calls it gave up are still blocked,
// which bounds the goroutines a mount that never lets go can cost.
//
// It is a destructive command run unattended, so everything doubtful is
// reported with a reason instead: a mount the agent did not create (an
// existing mount, a shared directory, another export or a second mount on
// the path), one with another mount below it (a lazy unmount would detach
// that too), one anything on the node reads or writes (Options.InUse), and
// one whose server holds another NFS mount of this node that is not itself
// stuck and unused (the forced unmount aborts requests per NFS superblock,
// which mounts of one server can share).
func (m *Manager) recoverStuck(ctx context.Context, sp Spec, st Status, sharePath bool) Status {
	now := m.o.Now()
	path := filepath.Clean(sp.Path)
	m.mu.Lock()
	state, known := m.stuck[sp.ID]
	if !known {
		state = stuckSince{since: now}
	}
	state.path, state.eligible = path, false
	m.stuck[sp.ID] = state
	m.mu.Unlock()
	st.Message += stuckSinceText(state)
	if !sharePath || sp.Type == TypeLocal {
		return st // a local disk: there is no mount to take off
	}
	if st.Reason == "" {
		st.Reason = ReasonUnresponsiveRetrying
	}
	started, held := m.g.blockedSince(storageKey(sp.ID))
	if !held || now.Sub(started) < 2*m.o.CallTimeout || m.mountRunning(path) {
		// It answers, only too slowly; or this check's own call is all that
		// waits; or the mount command itself has not returned, which is not
		// retried while it runs.
		return st
	}
	entries, err := m.readMounts()
	if err != nil {
		return st
	}
	entry, mounted := FindMount(entries, path)
	m.mu.Lock()
	record, ours := m.mounted[path]
	m.mu.Unlock()
	managed := (sp.Type == TypeNFS || sp.Type == TypeNFSCache) && !sp.Existing
	blockedFor := now.Sub(started)
	due := blockedFor >= m.o.StuckRemountAfter && (state.forced.IsZero() || now.Sub(state.forced) >= m.o.StuckRemountAfter)
	keys := func(key string) bool {
		return key == storageKey(sp.ID) || strings.HasPrefix(key, opKey(sp.ID, ""))
	}
	capped := m.g.orphaned(keys) >= m.o.StuckMaxAbandoned
	log := m.o.Logger.With("storage", sp.Name, "path", path, "since", state.since)
	switch {
	case !managed:
		st.Reason = ReasonStuckNotManaged
		return st
	case !mounted:
		// Nothing is mounted on the path any more (someone unmounted it, or a
		// forced unmount detached it and then failed): the blocked calls are
		// on a filesystem that is gone, and mounting takes nothing away.
		switch {
		case capped:
			st.Reason = ReasonStuckRestartNeeded
			return st
		case !due:
			st.Reason = ReasonStuckWillRemount
			return st
		}
		m.markForced(sp.ID, now)
		log.Warn("storagemount: an unresponsive mount is gone while calls are still blocked on it, mounting it again")
		return m.remountAfterStuck(ctx, sp, state, keys)
	case !ours || normalizeSource(record.Export) != normalizeSource(entry.Source) || !strings.HasPrefix(entry.FSType, "nfs") || mountsAt(entries, path) != 1:
		st.Reason = ReasonStuckNotManaged
		return st
	case mountBelow(entries, path):
		st.Reason = ReasonStuckNestedMount
		return st
	case m.o.InUse(sp.ID, path):
		st.Reason = ReasonStuckInUse
		return st
	case capped:
		st.Reason = ReasonStuckRestartNeeded
		return st
	}
	if blockedFor >= m.o.StuckRemountAfter {
		m.mu.Lock()
		state.eligible = true
		m.stuck[sp.ID] = state
		m.mu.Unlock()
	}
	shared, wait := m.serverSharedWithLiveMount(entries, entry, path)
	if shared {
		st.Reason = ReasonStuckSharedServer
		return st
	}
	st.Reason = ReasonStuckWillRemount
	if !due || wait {
		return st
	}
	m.markForced(sp.ID, now)
	// One forced unmount and remount at a time: the storages of one server
	// are forced off in the same pass, and the forced unmount of one must not
	// run while another's new mount is being checked.
	m.forceMu.Lock()
	defer m.forceMu.Unlock()
	out, cmdErr, err := m.runForcedUmount(ctx, "m:"+path, path)
	if err != nil || cmdErr != nil {
		detail := "it did not return"
		if err == nil {
			detail = truncate(strings.TrimSpace(string(out)), 200)
			if detail == "" {
				detail = cmdErr.Error()
			}
		}
		// umount can detach the mount point and still fail or be killed (it
		// talks to the server afterwards); what counts is the mount table.
		after, readErr := m.readMounts()
		if _, still := FindMount(after, path); readErr != nil || still {
			log.Warn("storagemount: forced unmount of an unresponsive mount failed", "detail", detail)
			st.Message += "; forced unmount failed: " + detail
			return st
		}
		log.Warn("storagemount: forced unmount reported a failure but the mount is gone", "detail", detail)
	}
	log.Warn("storagemount: forced an unresponsive mount off, mounting it again", "export", record.Export)
	return m.remountAfterStuck(ctx, sp, state, keys)
}

// remountAfterStuck gives up the calls still blocked on a mount that is no
// longer there and makes the storage usable again in the same pass.
func (m *Manager) remountAfterStuck(ctx context.Context, sp Spec, state stuckSince, keys func(string) bool) Status {
	abandoned := m.g.abandon(keys)
	m.forgetMount(filepath.Clean(sp.Path))
	m.o.Logger.Warn("storagemount: gave up the calls blocked on the old mount", "storage", sp.Name, "calls_given_up", abandoned)
	again, _ := m.reconcileSpec(ctx, sp)
	if again.State != StateUnresponsive {
		m.clearStuck(sp.ID)
		return again
	}
	// Still no answer (the new mount hangs too): reported as it is, and the
	// next forced unmount waits for a call blocked a full StuckRemountAfter.
	again.Reason = ReasonUnresponsiveRetrying
	again.Message += stuckSinceText(state)
	return again
}

func stuckSinceText(state stuckSince) string {
	return "; no answer since " + state.since.UTC().Format(time.RFC3339)
}

func (m *Manager) markForced(id uint, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.stuck[id]
	state.forced = now
	m.stuck[id] = state
}

// mountsAt counts the mounts stacked on exactly path.
func mountsAt(entries []MountEntry, path string) int {
	n := 0
	for _, e := range entries {
		if filepath.Clean(e.MountPoint) == path {
			n++
		}
	}
	return n
}

// mountBelow reports a mount point strictly inside path.
func mountBelow(entries []MountEntry, path string) bool {
	prefix := strings.TrimRight(path, "/") + "/"
	for _, e := range entries {
		if strings.HasPrefix(filepath.Clean(e.MountPoint)+"/", prefix) && filepath.Clean(e.MountPoint) != path {
			return true
		}
	}
	return false
}

// serverSharedWithLiveMount reports that the server of the stuck mount also
// backs another NFS mount on this node that a forced unmount might hurt: any
// one that was not found stuck, unused and the agent's own at the previous
// check (those lose nothing). wait is set when such stuck mounts
// exist and this one was not yet found forceable at the previous check: the
// mounts of one server are then forced off in the same pass, so none is
// forced after another has come back.
func (m *Manager) serverSharedWithLiveMount(entries []MountEntry, stuck MountEntry, path string) (shared, wait bool) {
	host := sourceHost(stuck.Source)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range entries {
		other := filepath.Clean(e.MountPoint)
		if other == path || !strings.HasPrefix(e.FSType, "nfs") || sourceHost(e.Source) != host {
			continue
		}
		// Judged by the previous check alone, not by the mount record: a
		// peer that is being forced off and mounted again in this same pass
		// is, for a moment, in the mount table without a record, and is no
		// more in use for it.
		if !m.wasEligible[other] {
			return true, false
		}
		wait = !m.wasEligible[path]
	}
	return false, wait
}

// mountRunning reports that a mount command for path has not returned.
func (m *Manager) mountRunning(path string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mounting[path] > 0
}

func (m *Manager) noteMounting(path string, delta int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mounting[path] += delta
}

// sourceHost is the server part of an NFS source ("host:/export"), lower
// case; a source without one is its own host.
func sourceHost(source string) string {
	if i := strings.Index(source, ":/"); i >= 0 {
		source = source[:i]
	}
	return strings.ToLower(strings.TrimSpace(source))
}

// snapshotEligible records, before a pass, which stuck mounts the pass
// before it found forceable on their own, so storages checked side by side
// judge each other by the same picture.
func (m *Manager) snapshotEligible() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.wasEligible = map[string]bool{}
	for _, state := range m.stuck {
		if state.eligible {
			m.wasEligible[state.path] = true
		}
	}
}

// forgetStuck drops the record of storages that are no longer listed.
func (m *Manager) forgetStuck(keep map[uint]bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.stuck {
		if !keep[id] {
			delete(m.stuck, id)
		}
	}
}

func (m *Manager) clearStuck(id uint) {
	m.mu.Lock()
	delete(m.stuck, id)
	m.mu.Unlock()
}

// runForcedUmount runs "umount -f -l path" under the guard: -f has the NFS
// client fail the requests it is waiting on, -l detaches the mount point
// while files on it are still open. Only recoverStuck uses it.
func (m *Manager) runForcedUmount(ctx context.Context, key, path string) (out []byte, cmdErr error, err error) {
	var o []byte
	var ce error
	err = m.g.run(ctx, key, m.o.MountTimeout+m.o.CallTimeout, func() error {
		cctx, cancel := context.WithTimeout(ctx, m.o.MountTimeout)
		defer cancel()
		o, ce = m.o.Runner(cctx, "umount", "-f", "-l", path)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return o, ce, nil
}
