package store

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"gorm.io/gorm"
)

// migrateSyncGroupNames keeps every group and its selector/rate semantics.
// The oldest row keeps each normalized name; duplicates get an ID suffix
// that cannot collide with any original name, including a later row's name.
func migrateSyncGroupNames(db *gorm.DB) error {
	if !db.Migrator().HasTable(&SyncGroup{}) {
		return nil
	}
	var groups []SyncGroup
	if err := db.Order("id").Find(&groups).Error; err != nil {
		return err
	}
	reserved, used := map[string]bool{}, map[string]bool{}
	for _, group := range groups {
		reserved[strings.TrimSpace(group.Name)] = true
	}
	if err := db.Exec("DROP INDEX IF EXISTS idx_sync_groups_name").Error; err != nil {
		return err
	}
	for _, group := range groups {
		name := strings.TrimSpace(group.Name)
		if name == "" || used[name] {
			base := name
			if base == "" {
				base = "sync-group"
			}
			for attempt := 0; ; attempt++ {
				suffix := " (#" + strconv.FormatUint(uint64(group.ID), 10)
				if attempt > 0 {
					suffix += "-" + strconv.Itoa(attempt)
				}
				suffix += ")"
				name = syncGroupNamePrefix(base, syncGroupNameMaxBytes-len(suffix)) + suffix
				if !reserved[name] && !used[name] {
					break
				}
			}
		}
		if name != group.Name {
			if err := db.Model(&SyncGroup{}).Where("id = ?", group.ID).UpdateColumn("name", name).Error; err != nil {
				return fmt.Errorf("rename duplicate sync group %d: %w", group.ID, err)
			}
		}
		used[name] = true
	}
	return db.Exec("CREATE UNIQUE INDEX idx_sync_groups_name ON sync_groups(name COLLATE BINARY)").Error
}

func syncGroupNamePrefix(name string, maximum int) string {
	if len(name) <= maximum {
		return name
	}
	name = name[:maximum]
	for !utf8.ValidString(name) {
		name = name[:len(name)-1]
	}
	return name
}
