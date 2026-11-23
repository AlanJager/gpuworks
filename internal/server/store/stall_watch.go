package store

import (
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/stallwatch"
)

// StallSite is the name a database statement has in a stall report.
const StallSite = "database statement"

const stallWatchKey = "gpuworks:stall_watch"

// watchStatements tells the process's stall watch how long each statement of
// db has been waiting: for a connection, for the single writer, for the disk
// or, on a replicated store, for the database process. A statement that
// outlasts the watch's threshold is what puts the stacks of everything
// waiting behind it into a report.
func watchStatements(db *gorm.DB) error {
	begin := func(tx *gorm.DB) {
		tx.InstanceSet(stallWatchKey, stallwatch.Begin(StallSite))
	}
	end := func(tx *gorm.DB) {
		if done, ok := tx.InstanceGet(stallWatchKey); ok {
			done.(func())()
		}
	}
	callbacks := db.Callback()
	for _, step := range []struct {
		before, after func(name string, fn func(*gorm.DB)) error
	}{
		{callbacks.Create().Before("*").Register, callbacks.Create().After("*").Register},
		{callbacks.Query().Before("*").Register, callbacks.Query().After("*").Register},
		{callbacks.Update().Before("*").Register, callbacks.Update().After("*").Register},
		{callbacks.Delete().Before("*").Register, callbacks.Delete().After("*").Register},
		{callbacks.Row().Before("*").Register, callbacks.Row().After("*").Register},
		{callbacks.Raw().Before("*").Register, callbacks.Raw().After("*").Register},
	} {
		if err := step.before("gpuworks:stall_watch_begin", begin); err != nil {
			return err
		}
		if err := step.after("gpuworks:stall_watch_end", end); err != nil {
			return err
		}
	}
	return nil
}
