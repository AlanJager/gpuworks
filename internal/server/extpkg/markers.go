package extpkg

import (
	"errors"
	"fmt"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Markers are the persisted version markers; *store.Store satisfies it.
type Markers interface {
	PluginMarkers() ([]extapi.Marker, error)
	SetPluginMarker(m extapi.Marker) error
	DeletePluginMarker(name string) error
}

// CheckDataMarker refuses a build older than the data its plugin already
// wrote: the stored data marker of the package may not be newer than the
// build's storage.state_version (0 for a build without storage). Install
// runs it before anything is unpacked, and the server again before every
// start, since a newer build may have run after this one was installed.
func (h *Host) CheckDataMarker(m *extapi.Manifest) error {
	name := extapi.PluginDataMarker(m.ID)
	own, ok, err := h.dataMarker(m.ID)
	if err != nil || !ok {
		return err
	}
	return extapi.CheckMarkers([]extapi.Marker{own}, map[string]int{name: stateVersion(m)})
}

// ErrDirBusy is RecordDataMarker's answer while an install or removal
// holds the plugins directory: nothing was recorded, and the caller tries
// again later instead of waiting.
var ErrDirBusy = errors.New("plugins directory is busy with an install or removal")

// RecordDataMarker records that package id's plugin has run healthy with
// data at stateVersion: the data marker becomes max(stored, stateVersion),
// so it is never lowered, and an older build cannot be installed or
// started over data a newer one wrote. A build without storage
// (stateVersion 0) writes no marker, and neither does a package no longer
// installed, so a purge is not undone by a plugin that was still stopping.
// writtenBy is the version of the plugin build that ran, which a refused
// older build names as the one to install instead. It never waits for the
// plugins directory: while an install or removal holds it, it returns
// ErrDirBusy, so a caller on the plugin supervisor is held only by the
// database read and write.
func (h *Host) RecordDataMarker(id string, stateVersion int, writtenBy string) error {
	if stateVersion < 1 || !extapi.ValidPackageID(id) {
		return nil
	}
	unlock, ok, err := h.Dir.TryLock()
	if err != nil {
		return err
	}
	if !ok {
		return ErrDirBusy
	}
	defer func() { _ = unlock() }()
	if version, err := h.Dir.Current(id); err != nil || version == "" {
		return err
	}
	own, ok, err := h.dataMarker(id)
	if err != nil || (ok && own.Version >= stateVersion) {
		return err
	}
	return h.Markers.SetPluginMarker(extapi.Marker{Name: extapi.PluginDataMarker(id), Version: stateVersion, WrittenBy: writtenBy})
}

// dataMarker is package id's stored data marker, if it has one.
func (h *Host) dataMarker(id string) (extapi.Marker, bool, error) {
	stored, err := h.Markers.PluginMarkers()
	if err != nil {
		return extapi.Marker{}, false, fmt.Errorf("read plugin version markers: %w", err)
	}
	name := extapi.PluginDataMarker(id)
	for _, s := range stored {
		if s.Name == name {
			return s, true, nil
		}
	}
	return extapi.Marker{}, false, nil
}

// stateVersion is the build's storage.state_version, 0 without storage.
func stateVersion(m *extapi.Manifest) int {
	if m.Storage == nil {
		return 0
	}
	return m.Storage.StateVersion
}
