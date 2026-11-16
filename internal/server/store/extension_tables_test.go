package store

import (
	"errors"
	"path/filepath"
	"testing"

	"gorm.io/gorm"
)

type quotaItem struct {
	ID  uint
	Key string
}

func (quotaItem) TableName() string { return "ee_quota_items" }

func TestExtensionTablesWriteAndReadTheNamespacesTables(t *testing.T) {
	for name, dsn := range map[string]string{
		"file":      filepath.Join(t.TempDir(), "gpuworks.db"),
		"in-memory": "file:" + t.Name() + "?mode=memory&cache=shared",
	} {
		t.Run(name, func(t *testing.T) {
			s := mustOpenWithSets(t, dsn, quotaSet())
			tables, err := s.ExtensionTables("quota")
			if err != nil {
				t.Fatal(err)
			}
			if tables.Namespace() != "quota" || tables.TablePrefix() != "ee_quota_" {
				t.Fatalf("handle = %s %s", tables.Namespace(), tables.TablePrefix())
			}
			if name == "file" && tables.reader == tables.writer {
				t.Fatal("a file database reads on the writer")
			}
			if err := tables.Write(func(tx *gorm.DB) error { return tx.Create(&quotaItem{Key: "k1"}).Error }); err != nil {
				t.Fatal(err)
			}
			// A failing write rolls back everything it did.
			injected := errors.New("injected")
			err = tables.Write(func(tx *gorm.DB) error {
				if err := tx.Create(&quotaItem{Key: "k2"}).Error; err != nil {
					return err
				}
				return injected
			})
			if !errors.Is(err, injected) {
				t.Fatalf("Write = %v, want the injected error", err)
			}
			var keys []string
			if err := tables.Read(func(db *gorm.DB) error { return db.Model(&quotaItem{}).Order("id").Pluck("key", &keys).Error }); err != nil {
				t.Fatal(err)
			}
			if len(keys) != 1 || keys[0] != "k1" {
				t.Fatalf("keys = %q, want only the committed k1", keys)
			}
			// The read pool cannot write, even by mistake.
			if name == "file" {
				if err := tables.Read(func(db *gorm.DB) error { return db.Create(&quotaItem{Key: "k3"}).Error }); err == nil {
					t.Fatal("a write through Read succeeded")
				}
			}
			if _, err := s.ExtensionTables("billing"); !errors.Is(err, ErrUnknownNamespace) {
				t.Fatalf("ExtensionTables(billing) = %v, want ErrUnknownNamespace", err)
			}
		})
	}
}
