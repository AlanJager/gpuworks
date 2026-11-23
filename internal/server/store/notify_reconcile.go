package store

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

var ErrNotifyConflict = errors.New("notification request conflicts with a committed resource")

type CommittedNotification struct {
	RequestKey string `gorm:"primaryKey;size:128"`
	ResourceID string `gorm:"size:64;not null;uniqueIndex:idx_notify_topic_resource,priority:2"`
	Topic      string `gorm:"size:32;not null;uniqueIndex:idx_notify_topic_resource,priority:1"`
	Reason     string `gorm:"size:32;not null"`
	Published  bool
	Revision   uint64 `gorm:"not null;default:0"`
}

type agentStatusNoticeKey struct{}

func (s *Store) CommitNotifiedInstance(ctx context.Context, requestKey, instanceUUID string, deliver bool) (*Instance, bool, error) {
	if requestKey == "" || instanceUUID == "" {
		return nil, false, errors.New("notification commit requires a request key and instance")
	}
	var inst Instance
	created := false
	err := s.schedulingWriter().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing CommittedNotification
		err := tx.First(&existing, "request_key = ?", requestKey).Error
		if err == nil {
			if existing.ResourceID != instanceUUID {
				return ErrNotifyConflict
			}
			return tx.Where("uuid = ?", existing.ResourceID).First(&inst).Error
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		inst = Instance{UUID: instanceUUID, State: proto.InstPending}
		if err := tx.Create(&inst).Error; err != nil {
			return err
		}
		note := CommittedNotification{RequestKey: requestKey, ResourceID: instanceUUID, Topic: "instance", Reason: "created"}
		if err := tx.Create(&note).Error; err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if deliver {
		if _, err := s.ReconcileDroppedNotifications(ctx); err != nil {
			return &inst, created, err
		}
	}
	return &inst, created, nil
}

// ReconcileDroppedNotifications publishes commits whose notify never left the
// process. It does not insert resources.
func (s *Store) ReconcileDroppedNotifications(ctx context.Context) (int, error) {
	return s.reconcileDroppedNotificationsBatch(ctx, 0)
}

// ReconcileDroppedNotificationsBatch bounds one recovery tick so a backlog
// cannot monopolize the controller's short notification timer.
func (s *Store) ReconcileDroppedNotificationsBatch(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		return 0, errors.New("notification batch limit must be positive")
	}
	return s.reconcileDroppedNotificationsBatch(ctx, limit)
}

func (s *Store) reconcileDroppedNotificationsBatch(ctx context.Context, limit int) (int, error) {
	published, scanned, err := s.reconcileDroppedNotifications(ctx, s.schedulingWriter(), limit)
	if err != nil || s.db == nil || s.schedulingDB == nil || s.schedulingDB == s.db {
		return published, err
	}
	if limit > 0 {
		limit -= scanned
		if limit == 0 {
			return published, nil
		}
	}
	extra, _, err := s.reconcileDroppedNotifications(ctx, s.db, limit)
	return published + extra, err
}

func (s *Store) reconcileDroppedNotifications(ctx context.Context, db *gorm.DB, limit int) (int, int, error) {
	if db == nil {
		return 0, 0, nil
	}
	var pending []CommittedNotification
	query := db.WithContext(ctx).Where("published = ?", false).Order("request_key")
	if limit > 0 {
		query = query.Limit(limit)
	}
	if err := query.Find(&pending).Error; err != nil {
		return 0, 0, err
	}
	published := 0
	for _, note := range pending {
		if s.Bus == nil {
			continue
		}
		s.Bus.PublishChange(note.Topic, note.ResourceID, note.Reason)
		count, err := s.ackCommittedNotification(ctx, db, note)
		if err != nil {
			return published, len(pending), err
		}
		published += int(count)
	}
	return published, len(pending), nil
}

// publishCommittedNotification publishes the latest committed notice for a
// key, then acknowledges exactly the revision it read. A caller that finished
// an older write may safely invoke this after a newer write has committed.
func (s *Store) publishCommittedNotification(key string) error {
	if s.Bus == nil || s.db == nil || key == "" {
		return nil
	}
	var note CommittedNotification
	err := s.db.First(&note, "request_key = ?", key).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil || note.Published {
		return err
	}
	s.Bus.PublishChange(note.Topic, note.ResourceID, note.Reason)
	// The bus is process-local. A follower, or a leader check that did not
	// answer, leaves the replicated notice pending for the leader's scanner.
	local, leaderErr := s.LocalSchedulingLeader(context.Background())
	if leaderErr != nil {
		local = false
	}
	if !local {
		return nil
	}
	_, err = s.ackCommittedNotification(context.Background(), s.schedulingWriter(), note)
	return err
}

// ackCommittedNotification confirms only the observed revision on the original
// pool. Marker errors do not alter the already committed target result.
func (s *Store) ackCommittedNotification(ctx context.Context, db *gorm.DB, note CommittedNotification) (int64, error) {
	var count int64
	write := func(tx *gorm.DB) error {
		res := tx.Model(&CommittedNotification{}).
			Where("request_key = ? AND published = ? AND revision = ?", note.RequestKey, false, note.Revision).
			Update("published", true)
		count = res.RowsAffected
		return res.Error
	}
	var err error
	if b := s.businessWriteBarrier; b != nil && b.holding() {
		// Already inside the admitted call. Do not take a second admission:
		// the commit that saved this notice may have closed the gate.
		err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := b.eligible(ctx); err != nil {
				return err
			}
			if err := requireBusinessServingMaintenancePhase(ctx, tx); err != nil {
				return err
			}
			if err := s.requireBusinessWriterAdmitted(ctx, tx); err != nil {
				return err
			}
			if err := write(tx); err != nil {
				return err
			}
			return readBusinessServingMaintenancePhase(ctx, tx)
		})
	} else {
		err = s.withBusinessWritePoolTx(ctx, db, write)
	}
	if err != nil {
		return 0, err
	}
	return count, nil
}

func nodeNotifyKey(uuid string) string {
	return "node:" + uuid
}

func imageNodeNotifyKey(imageVersionID uint) string {
	return "imagenode:" + uintKey(imageVersionID)
}

func modelNotifyKey(id uint) string {
	return "model:" + uintKey(id)
}

func apiKeyNotifyKey(prefix string) string {
	return "apikey:" + prefix
}

func settingNotifyKey(key string) string {
	return "setting:" + key
}

// saveUnpublishedNotification keeps one owed notification for a resource inside
// the caller's transaction. A repeated commit updates that row instead of
// inserting another resource.
func saveUnpublishedNotification(tx *gorm.DB, note CommittedNotification) error {
	var existing CommittedNotification
	err := tx.First(&existing, "request_key = ?", note.RequestKey).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		note.Revision = 1
		err = tx.Create(&note).Error
	} else if err == nil {
		if existing.ResourceID != note.ResourceID || existing.Topic != note.Topic {
			return ErrNotifyConflict
		}
		note.Revision = existing.Revision + 1
		err = tx.Model(&CommittedNotification{}).Where("request_key = ?", note.RequestKey).Updates(map[string]any{
			"reason": note.Reason, "published": false, "revision": note.Revision,
		}).Error
	}
	if err != nil {
		return err
	}
	if notices, ok := tx.Statement.Context.Value(agentStatusNoticeKey{}).(*[]CommittedNotification); ok {
		*notices = append(*notices, note)
	}
	return nil
}

func migrateNotificationRevision(db *gorm.DB) error {
	if db.Migrator().HasColumn(&CommittedNotification{}, "revision") {
		return nil
	}
	return db.Exec("ALTER TABLE committed_notifications ADD COLUMN revision INTEGER NOT NULL DEFAULT 0").Error
}

func migrateCommittedNotifications(db *gorm.DB) error {
	return db.AutoMigrate(&CommittedNotification{})
}

// migrateNotificationTopicResource lets two topics use the same bus key. A
// model id and a deployment id are both "1"; the owed notification is unique
// per topic and key.
func migrateNotificationTopicResource(db *gorm.DB) error {
	if err := db.Exec("DROP INDEX IF EXISTS idx_committed_notifications_resource_id").Error; err != nil {
		return err
	}
	return db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_notify_topic_resource ON committed_notifications(topic, resource_id)").Error
}
