package store

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"

	"gpuworks.ai/gpuworks/internal/server/store/pstate"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// The plugin host's trust (docs/reference/plugin-protocol.md, ADR 0071):
// project roots that trust updates added or retired beyond the ones compiled
// into the server, revocations, the highest serial applied per issuer of
// trust updates and revocation lists, the lowest version each package may be
// installed at, and the version markers of persisted plugin state. Each is a
// typed table; the plugin host (internal/server/extpkg/trust) reads and
// changes them through the methods below.

// PluginTrustRoot is a project root that a trust update added or retired.
type PluginTrustRoot struct {
	KeyID string `gorm:"primaryKey;size:64"`
	// Alg and PublicKey are the key; empty for a retired root that was
	// compiled into the server rather than added.
	Alg       string `gorm:"size:32"`
	PublicKey []byte
	Retired   bool
	// Serial is the trust update that last changed the row.
	Serial    uint64
	UpdatedAt time.Time
}

// TableName keeps the table name stable.
func (PluginTrustRoot) TableName() string { return "plugin_trust_roots" }

// PluginRevocation is one revocation entry. Anchor is the trust anchor its
// list applies under ("gwroot" for the project roots, "host:<keyid>" for an
// issuer the host operator trusts); an entry by manifest digest applies under
// every anchor.
type PluginRevocation struct {
	ID          uint   `gorm:"primaryKey"`
	Anchor      string `gorm:"size:80;not null;uniqueIndex:idx_plugin_revocations_entry"`
	Publisher   string `gorm:"size:16;not null;uniqueIndex:idx_plugin_revocations_entry"`
	CertSerial  uint64 `gorm:"not null;default:0;uniqueIndex:idx_plugin_revocations_entry"`
	KeyID       string `gorm:"size:64;not null;default:'';uniqueIndex:idx_plugin_revocations_entry"`
	Manifest    string `gorm:"size:80;not null;default:'';uniqueIndex:idx_plugin_revocations_entry"`
	Replacement string `gorm:"size:128"`
	// Issuer and Serial name the list that carried the entry first.
	Issuer    string `gorm:"size:128"`
	Serial    uint64
	CreatedAt time.Time
}

// TableName keeps the table name stable.
func (PluginRevocation) TableName() string { return "plugin_trust_revocations" }

// PluginTrustSerial is the highest serial applied from one issuer of trust
// updates or revocation lists; a lower or equal one is a replay.
type PluginTrustSerial struct {
	Issuer    string `gorm:"primaryKey;size:128"`
	Serial    uint64
	UpdatedAt time.Time
}

// TableName keeps the table name stable.
func (PluginTrustSerial) TableName() string { return "plugin_trust_serials" }

// PluginFloor is the lowest version a package may be installed, upgraded or
// rolled back to. It only rises; uninstalling the package keeps it.
type PluginFloor struct {
	ID         string `gorm:"primaryKey;size:32"`
	MinVersion string `gorm:"size:128;not null"`
	UpdatedAt  time.Time
}

// TableName keeps the table name stable.
func (PluginFloor) TableName() string { return "plugin_floors" }

// PluginMarker is a persisted version marker (extapi.Marker): the format
// version of one kind of stored plugin state and the server version that
// wrote it. A server that finds one newer than it knows refuses to start.
type PluginMarker struct {
	Name      string `gorm:"primaryKey;size:96"`
	Version   int    `gorm:"not null"`
	WrittenBy string `gorm:"size:64"`
	UpdatedAt time.Time
}

// TableName keeps the table name stable.
func (PluginMarker) TableName() string { return "plugin_markers" }

// addPluginTrustTables creates the plugin host's trust and marker tables.
func addPluginTrustTables(db *gorm.DB) error {
	return db.AutoMigrate(&PluginTrustRoot{}, &PluginRevocation{}, &PluginTrustSerial{}, &PluginFloor{}, &PluginMarker{})
}

// PluginRoots returns the roots trust updates added and not retired since,
// sorted by key id, and the key ids they retired.
func (s *Store) PluginRoots() (added []extapi.PublicKey, retired []string, err error) {
	var rows []PluginTrustRoot
	if err := s.reader().Order("key_id").Find(&rows).Error; err != nil {
		return nil, nil, err
	}
	for _, r := range rows {
		if r.Retired {
			retired = append(retired, r.KeyID)
			continue
		}
		added = append(added, extapi.PublicKey{KeyID: r.KeyID, Alg: r.Alg, Key: r.PublicKey})
	}
	return added, retired, nil
}

// PluginRevocations returns every revocation by anchor.
func (s *Store) PluginRevocations() (map[string][]extapi.Revocation, error) {
	var rows []PluginRevocation
	if err := s.reader().Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := map[string][]extapi.Revocation{}
	for _, r := range rows {
		out[r.Anchor] = append(out[r.Anchor], extapi.Revocation{
			Publisher: r.Publisher, CertSerial: r.CertSerial, KeyID: r.KeyID,
			Manifest: extapi.Digest(r.Manifest), Replacement: r.Replacement,
		})
	}
	return out, nil
}

// PluginTrustSerials returns the highest serial applied per issuer.
func (s *Store) PluginTrustSerials() (map[string]uint64, error) {
	var rows []PluginTrustSerial
	if err := s.reader().Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]uint64, len(rows))
	for _, r := range rows {
		out[r.Issuer] = r.Serial
	}
	return out, nil
}

// PluginFloors returns the lowest installable version per package id.
func (s *Store) PluginFloors() (map[string]string, error) {
	var rows []PluginFloor
	if err := s.reader().Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.ID] = r.MinVersion
	}
	return out, nil
}

// ErrPluginRootConflict is returned when a trust change adds a root under a
// key id the table already holds with another key: a key id names one key
// forever.
var ErrPluginRootConflict = errors.New("plugin trust root key id already names another key")

// ApplyPluginTrust applies one verified trust update or revocation list (a
// list travels as an update carrying only Revoke) in one transaction:
// roots added and retired, revocations recorded under anchor, floors raised
// (never lowered), and u.Serial recorded as issuer's highest. A serial that
// is not above the highest already applied changes nothing: applied is false
// and highest is the serial on record.
func (s *Store) ApplyPluginTrust(issuer, anchor string, u extapi.TrustUpdate) (applied bool, highest uint64, err error) {
	err = s.db.Transaction(func(tx *gorm.DB) error {
		var row PluginTrustSerial
		switch err := tx.Where("issuer = ?", issuer).First(&row).Error; {
		case err == nil:
			if u.Serial <= row.Serial {
				highest = row.Serial
				return nil
			}
		case !errors.Is(err, gorm.ErrRecordNotFound):
			return err
		}
		now := time.Now().UTC()
		for _, k := range u.AddRoots {
			if err := addPluginRoot(tx, k, u.Serial, now); err != nil {
				return err
			}
		}
		for _, id := range u.RetireRoots {
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "key_id"}},
				DoUpdates: clause.Assignments(map[string]any{"retired": true, "serial": u.Serial, "updated_at": now}),
			}).Create(&PluginTrustRoot{KeyID: id, Retired: true, Serial: u.Serial, UpdatedAt: now}).Error; err != nil {
				return err
			}
		}
		for _, r := range u.Revoke {
			rev := PluginRevocation{Anchor: anchor, Publisher: r.Publisher, CertSerial: r.CertSerial, KeyID: r.KeyID,
				Manifest: string(r.Manifest), Replacement: r.Replacement, Issuer: issuer, Serial: u.Serial, CreatedAt: now}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&rev).Error; err != nil {
				return err
			}
		}
		for _, f := range u.Floors {
			if _, err := raisePluginFloor(tx, f.ID, f.MinVersion, now); err != nil {
				return err
			}
		}
		if err := tx.Save(&PluginTrustSerial{Issuer: issuer, Serial: u.Serial, UpdatedAt: now}).Error; err != nil {
			return err
		}
		applied, highest = true, u.Serial
		return nil
	})
	return applied, highest, err
}

func addPluginRoot(tx *gorm.DB, k extapi.PublicKey, serial uint64, now time.Time) error {
	var existing PluginTrustRoot
	switch err := tx.Where("key_id = ?", k.KeyID).First(&existing).Error; {
	case err == nil:
		if len(existing.PublicKey) > 0 && (existing.Alg != k.Alg || string(existing.PublicKey) != string(k.Key)) {
			return fmt.Errorf("%w: %s", ErrPluginRootConflict, k.KeyID)
		}
	case !errors.Is(err, gorm.ErrRecordNotFound):
		return err
	}
	return tx.Save(&PluginTrustRoot{KeyID: k.KeyID, Alg: k.Alg, PublicKey: k.Key, Serial: serial, UpdatedAt: now}).Error
}

// RaisePluginFloor raises the floor of package id to version, unless the
// floor is already at or above it; raised reports whether it changed.
func (s *Store) RaisePluginFloor(id, version string) (raised bool, err error) {
	err = s.db.Transaction(func(tx *gorm.DB) error {
		raised, err = raisePluginFloor(tx, id, version, time.Now().UTC())
		return err
	})
	return raised, err
}

func raisePluginFloor(tx *gorm.DB, id, version string, now time.Time) (bool, error) {
	if _, err := extapi.CompareVersions(version, version); err != nil {
		return false, fmt.Errorf("plugin floor of %s: %w", id, err)
	}
	var row PluginFloor
	switch err := tx.Where("id = ?", id).First(&row).Error; {
	case err == nil:
		// A stored floor this build cannot parse is replaced, never kept
		// below a valid one.
		if c, err := extapi.CompareVersions(version, row.MinVersion); err == nil && c <= 0 {
			return false, nil
		}
	case !errors.Is(err, gorm.ErrRecordNotFound):
		return false, err
	}
	return true, tx.Save(&PluginFloor{ID: id, MinVersion: version, UpdatedAt: now}).Error
}

// PluginMarkers returns every persisted version marker, sorted by name:
// those of the plugin host and package data (plugin_markers) and those of
// pushed state (pstate_marker), so one check sees every format a newer
// core may have written.
func (s *Store) PluginMarkers() ([]extapi.Marker, error) {
	return readPluginMarkers(s.reader())
}

func readPluginMarkers(db *gorm.DB) ([]extapi.Marker, error) {
	var rows []PluginMarker
	if err := db.Order("name").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]extapi.Marker, len(rows))
	for i, r := range rows {
		out[i] = extapi.Marker{Name: r.Name, Version: r.Version, WrittenBy: r.WrittenBy}
	}
	if !db.Migrator().HasTable(&pstate.Marker{}) {
		return out, nil
	}
	var pushed []pstate.Marker
	if err := db.Order("name").Find(&pushed).Error; err != nil {
		return nil, err
	}
	for _, r := range pushed {
		out = append(out, extapi.Marker{Name: r.Name, Version: r.Version, WrittenBy: r.WrittenBy})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ReadPluginMarkers reads the version markers of the database at dsn
// without migrating it and without writing: a server about to be replaced
// by an older one checks with it whether that one would start
// (`gpuworks plugin compat`). A database without the marker table (written
// before markers existed) has none.
func ReadPluginMarkers(dsn string) ([]extapi.Marker, error) {
	path, err := DatabaseFilePath(dsn)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, errors.New("plugin markers are read from a database file")
	}
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	gdb, err := gorm.Open(sqlite.Open(readOnlyDSN(dsn)), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil, err
	}
	watchDatabase(gdb)
	pool, err := gdb.DB()
	if err != nil {
		return nil, err
	}
	defer pool.Close()
	if !gdb.Migrator().HasTable(&PluginMarker{}) {
		return nil, nil
	}
	return readPluginMarkers(gdb)
}

// SetPluginMarker records m, replacing the marker of the same name.
func (s *Store) SetPluginMarker(m extapi.Marker) error {
	return s.db.Save(&PluginMarker{Name: m.Name, Version: m.Version, WrittenBy: m.WrittenBy, UpdatedAt: time.Now().UTC()}).Error
}

// DeletePluginMarker removes the marker named name, if any.
func (s *Store) DeletePluginMarker(name string) error {
	return s.db.Where("name = ?", name).Delete(&PluginMarker{}).Error
}
