package statsdb

import (
	"context"
	"database/sql"
)

// The scheduling activity's hours and days (docs/reference/api.md, section
// 调度活动): how many scheduling events of each reason, how often each card
// changed hands, and each deployment that shares cards' awake and present
// time and switches. The last 24 hours are counted in memory
// (internal/server/schedactivity); an hour is written whole once it is over,
// replacing what an earlier run wrote for it, so a restart that counted the
// hour again from the event log writes the same row.

// schemaV3 adds the scheduling activity's tables. A reason is
// store.ActivityReason's key; a deployment row is one of a deployment that
// shares cards (store.ListedInSharing), whose deletion and name are read
// from the main database, not kept here.
var schemaV3 = []string{
	`CREATE TABLE sched_reason_hourly (
		hour_ms INTEGER NOT NULL, reason TEXT NOT NULL, events INTEGER NOT NULL,
		PRIMARY KEY (hour_ms, reason))`,
	`CREATE TABLE sched_reason_daily (
		day INTEGER NOT NULL, reason TEXT NOT NULL, events INTEGER NOT NULL,
		PRIMARY KEY (day, reason))`,
	`CREATE TABLE sched_card_hourly (
		hour_ms INTEGER NOT NULL, node_uuid TEXT NOT NULL, gpu_index INTEGER NOT NULL, switches INTEGER NOT NULL,
		PRIMARY KEY (hour_ms, node_uuid, gpu_index))`,
	`CREATE TABLE sched_card_daily (
		day INTEGER NOT NULL, node_uuid TEXT NOT NULL, gpu_index INTEGER NOT NULL, switches INTEGER NOT NULL,
		PRIMARY KEY (day, node_uuid, gpu_index))`,
	`CREATE TABLE sched_deployment_hourly (
		hour_ms INTEGER NOT NULL, deployment_id INTEGER NOT NULL,
		awake_seconds INTEGER NOT NULL, present_seconds INTEGER NOT NULL,
		yielded INTEGER NOT NULL, yielded_to INTEGER NOT NULL, wakes INTEGER NOT NULL,
		PRIMARY KEY (hour_ms, deployment_id))`,
	`CREATE TABLE sched_deployment_daily (
		day INTEGER NOT NULL, deployment_id INTEGER NOT NULL,
		awake_seconds INTEGER NOT NULL, present_seconds INTEGER NOT NULL,
		yielded INTEGER NOT NULL, yielded_to INTEGER NOT NULL, wakes INTEGER NOT NULL,
		PRIMARY KEY (day, deployment_id))`,
}

// schedulingDayRollups write a day's scheduling rows from its hours (?1 the
// day, [?2, ?3) its hours).
var schedulingDayRollups = []string{
	`DELETE FROM sched_reason_daily WHERE day = ?1`,
	`INSERT INTO sched_reason_daily SELECT ?1, reason, SUM(events)
		FROM sched_reason_hourly WHERE hour_ms >= ?2 AND hour_ms < ?3 GROUP BY reason`,
	`DELETE FROM sched_card_daily WHERE day = ?1`,
	`INSERT INTO sched_card_daily SELECT ?1, node_uuid, gpu_index, SUM(switches)
		FROM sched_card_hourly WHERE hour_ms >= ?2 AND hour_ms < ?3 GROUP BY node_uuid, gpu_index`,
	`DELETE FROM sched_deployment_daily WHERE day = ?1`,
	`INSERT INTO sched_deployment_daily SELECT ?1, deployment_id, SUM(awake_seconds), SUM(present_seconds),
		SUM(yielded), SUM(yielded_to), SUM(wakes)
		FROM sched_deployment_hourly WHERE hour_ms >= ?2 AND hour_ms < ?3 GROUP BY deployment_id`,
}

// SchedulingHour is one hour of the scheduling activity, written whole.
type SchedulingHour struct {
	HourMS int64
	// Reasons counts the events of each reason (store.ActivityReason);
	// a reason with none is left out.
	Reasons     map[string]int64
	Cards       []SchedulingCard
	Deployments []SchedulingDeployment
}

// SchedulingCard is a card's switches: how often it changed hands (a yield
// or a wake on it).
type SchedulingCard struct {
	NodeUUID string
	GPU      int
	Switches int64
}

// SchedulingDeployment is a deployment's hour (or day, or range).
type SchedulingDeployment struct {
	DeploymentID   uint
	AwakeSeconds   int64
	PresentSeconds int64
	Yielded        int64
	YieldedTo      int64
	Wakes          int64
}

// ReplaceSchedulingHours writes hours whole, each replacing the rows written
// for it before, in one transaction.
func (s *Store) ReplaceSchedulingHours(ctx context.Context, hours []SchedulingHour) error {
	if len(hours) == 0 {
		return nil
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, table := range []string{"sched_reason_hourly", "sched_card_hourly", "sched_deployment_hourly"} {
			stmt, err := tx.PrepareContext(ctx, `DELETE FROM `+table+` WHERE hour_ms = ?`)
			if err != nil {
				return err
			}
			for _, h := range hours {
				if _, err := stmt.ExecContext(ctx, h.HourMS); err != nil {
					stmt.Close()
					return err
				}
			}
			stmt.Close()
		}
		reason, err := tx.PrepareContext(ctx, `INSERT INTO sched_reason_hourly (hour_ms, reason, events) VALUES (?, ?, ?)`)
		if err != nil {
			return err
		}
		defer reason.Close()
		card, err := tx.PrepareContext(ctx, `INSERT INTO sched_card_hourly (hour_ms, node_uuid, gpu_index, switches) VALUES (?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer card.Close()
		dep, err := tx.PrepareContext(ctx, `INSERT INTO sched_deployment_hourly (hour_ms, deployment_id,
			awake_seconds, present_seconds, yielded, yielded_to, wakes) VALUES (?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer dep.Close()
		for _, h := range hours {
			for key, n := range h.Reasons {
				if n == 0 {
					continue
				}
				if _, err := reason.ExecContext(ctx, h.HourMS, key, n); err != nil {
					return err
				}
			}
			for _, c := range h.Cards {
				if _, err := card.ExecContext(ctx, h.HourMS, c.NodeUUID, c.GPU, c.Switches); err != nil {
					return err
				}
			}
			for _, d := range h.Deployments {
				if _, err := dep.ExecContext(ctx, h.HourMS, d.DeploymentID, d.AwakeSeconds, d.PresentSeconds, d.Yielded, d.YieldedTo, d.Wakes); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// SchedulingReasonRow is one bucket's events of one reason.
type SchedulingReasonRow struct {
	Bucket int64
	Reason string
	Events int64
}

// SchedulingReasons reads the events per bucket and reason over r: at most
// one row per hour (or day) and reason.
func (s *Store) SchedulingReasons(ctx context.Context, r Range) ([]SchedulingReasonRow, error) {
	table, column := r.table("sched_reason")
	return scanRows(ctx, s.reader, `SELECT `+column+`, reason, events FROM `+table+` WHERE `+column+` >= ? AND `+column+` < ?`,
		[]any{r.From, r.To}, func(rows *sql.Rows) (SchedulingReasonRow, error) {
			var row SchedulingReasonRow
			err := rows.Scan(&row.Bucket, &row.Reason, &row.Events)
			return row, err
		})
}

// SchedulingCards sums each card's switches over r, one row per card that
// switched: a range of the primary key, grouped.
func (s *Store) SchedulingCards(ctx context.Context, r Range) ([]SchedulingCard, error) {
	table, column := r.table("sched_card")
	return scanRows(ctx, s.reader, `SELECT node_uuid, gpu_index, SUM(switches) FROM `+table+` WHERE `+column+` >= ? AND `+column+
		` < ? GROUP BY node_uuid, gpu_index`, []any{r.From, r.To}, func(rows *sql.Rows) (SchedulingCard, error) {
		var c SchedulingCard
		err := rows.Scan(&c.NodeUUID, &c.GPU, &c.Switches)
		return c, err
	})
}

// SchedulingDeployments sums each deployment's figures over r.
func (s *Store) SchedulingDeployments(ctx context.Context, r Range) ([]SchedulingDeployment, error) {
	table, column := r.table("sched_deployment")
	return scanRows(ctx, s.reader, `SELECT deployment_id, SUM(awake_seconds), SUM(present_seconds), SUM(yielded), SUM(yielded_to), SUM(wakes)
		FROM `+table+` WHERE `+column+` >= ? AND `+column+` < ? GROUP BY deployment_id`, []any{r.From, r.To},
		func(rows *sql.Rows) (SchedulingDeployment, error) {
			var d SchedulingDeployment
			err := rows.Scan(&d.DeploymentID, &d.AwakeSeconds, &d.PresentSeconds, &d.Yielded, &d.YieldedTo, &d.Wakes)
			return d, err
		})
}
