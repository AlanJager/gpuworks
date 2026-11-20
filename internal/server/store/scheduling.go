package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// SchedulingTx exposes only the reads and writes used by a placement decision.
// Its private store routes every query (including reader()) to one transaction.
// Optional lookups retain their not-found semantics; other read errors poison
// the decision even when a legacy estimator uses a conservative fallback.
type SchedulingTx struct {
	store         *Store
	err           error
	created       []string
	updatedModels []uint
}

// schedulingTransactionTimeout bounds database work and lock ownership. The
// callback must honor cancellation and must not perform external I/O; a Go
// context cannot preempt arbitrary CPU work inside a callback.
const schedulingTransactionTimeout = 5 * time.Second

// WithSchedulingTx owns the outer transaction. The callback must be synchronous;
// its handle expires on return and must not be shared with another goroutine.
func (s *Store) WithSchedulingTx(ctx context.Context, fn func(*SchedulingTx) error) error {
	ctx, cancel := context.WithTimeout(ctx, schedulingTransactionTimeout)
	defer cancel()
	if err := s.rejectKnownRemoteSchedulingLeader(ctx); err != nil {
		return err
	}
	epoch, err := s.observedSchedulingGeneration(ctx)
	if err != nil {
		return err
	}
	var decision *SchedulingTx
	err = s.schedulingWriteTransaction(ctx, func(tx *gorm.DB) error {
		decision = &SchedulingTx{store: &Store{db: tx, readDB: tx, secretbox: s.secretbox}}
		if err := s.rejectStaleSchedulingEpoch(tx, epoch); err != nil {
			return err
		}
		if err := fn(decision); err != nil {
			return err
		}
		if decision.err != nil {
			return decision.err
		}
		return s.rejectStaleSchedulingEpoch(tx, epoch)
	})
	if err != nil {
		return err
	}
	if s.Bus != nil && decision != nil {
		for _, uuid := range decision.created {
			if uuid == "" {
				continue
			}
			_ = s.publishCommittedNotification(instanceNotifyKey(uuid))
		}
		for _, id := range decision.updatedModels {
			s.publishModelWeight(id)
		}
	}
	return nil
}

func instanceNotifyKey(uuid string) string {
	return "instance:" + uuid
}

func (t *SchedulingTx) check(err error) {
	if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, gorm.ErrRecordNotFound) && t.err == nil {
		t.err = err
	}
}

func (t *SchedulingTx) GetDeployment(id uint) (*Deployment, error) {
	v, e := t.store.GetDeployment(id)
	t.check(e)
	return v, e
}
func (t *SchedulingTx) NodeByID(id uint) (*Node, error) {
	v, e := t.store.NodeByID(id)
	t.check(e)
	return v, e
}
func (t *SchedulingTx) GetModel(id uint) (*Model, error) {
	v, e := t.store.GetModel(id)
	t.check(e)
	return v, e
}
func (t *SchedulingTx) InstancesForNode(id uint) ([]Instance, error) {
	v, e := t.store.InstancesForNode(id)
	t.check(e)
	return v, e
}
func (t *SchedulingTx) InstancesForDeployment(id uint) ([]Instance, error) {
	v, e := t.store.InstancesForDeployment(id)
	t.check(e)
	return v, e
}
func (t *SchedulingTx) ModelFileByModelNode(model, node uint) (*ModelFile, error) {
	v, e := t.store.ModelFileByModelNode(model, node)
	t.check(e)
	return v, e
}
func (t *SchedulingTx) ResolveImageRef(ref string) (*Image, error) {
	v, e := t.store.ResolveImageRef(ref)
	t.check(e)
	return v, e
}
func (t *SchedulingTx) ImageNode(image, node uint) (*ImageNode, error) {
	v, e := t.store.ImageNode(image, node)
	t.check(e)
	return v, e
}
func (t *SchedulingTx) FillModelWeight(id uint, weight int64) error {
	changed, e := fillModelWeight(t.store.db, id, weight)
	t.check(e)
	if e == nil && changed {
		t.updatedModels = append(t.updatedModels, id)
	}
	return e
}

// FillModelWeight records a catalog fact only while the stored value is unknown.
// A preview must not overwrite metadata which arrived after its initial read.
func (s *Store) FillModelWeight(id uint, weight int64) error {
	changed := false
	err := s.withBusinessWriteTx(context.Background(), func(tx *gorm.DB) error {
		var err error
		changed, err = fillModelWeight(tx, id, weight)
		return err
	})
	if err == nil && changed {
		s.publishModelWeight(id)
	}
	return err
}

func fillModelWeight(tx *gorm.DB, id uint, weight int64) (bool, error) {
	result := tx.Model(&Model{}).Where("id = ? AND weight_bytes <= 0", id).Update("weight_bytes", weight)
	if result.Error != nil || result.RowsAffected == 0 {
		return false, result.Error
	}
	if err := saveUnpublishedNotification(tx, CommittedNotification{
		RequestKey: modelNotifyKey(id), ResourceID: uintKey(id), Topic: "model", Reason: "weight_discovered",
	}); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) publishModelWeight(id uint) {
	if s.Bus == nil {
		return
	}
	_ = s.publishCommittedNotification(modelNotifyKey(id))
}

func (t *SchedulingTx) GetSetting(key SettingKey) string {
	var setting Setting
	e := t.store.db.First(&setting, "key = ?", string(key)).Error
	t.check(e)
	return setting.Value
}
func (t *SchedulingTx) CreateInstance(i *Instance) error {
	if t.err != nil {
		return t.err
	}
	if i.State == "" {
		i.State = proto.InstPending
	}
	if e := t.store.db.Create(i).Error; e != nil {
		t.err = e
		return e
	}
	data, _ := json.Marshal(map[string]any{"state": i.State})
	e := t.store.db.Create(&InstanceEvent{InstanceID: i.ID, InstanceUUID: i.UUID, DeploymentID: i.DeploymentID, Kind: "state", Code: instanceEventCreated, Data: string(data), CreatedAt: time.Now().UTC()}).Error
	if e != nil {
		t.err = e
		return e
	}
	if i.UUID != "" {
		note := CommittedNotification{RequestKey: instanceNotifyKey(i.UUID), ResourceID: i.UUID, Topic: "instance", Reason: "created"}
		if e := t.store.db.Create(&note).Error; e != nil {
			t.err = e
			return e
		}
	}
	t.created = append(t.created, i.UUID)
	return nil
}

// RecordUnschedulable persists the bounded scheduling timeline entry in the
// decision transaction and returns the delay for the next local wakeup.
func (t *SchedulingTx) RecordUnschedulable(deployment uint, reason any) (time.Duration, error) {
	policy := DefaultDeploymentPolicy()
	raw := t.GetSetting(SettingDeploymentPolicy)
	if raw != "" {
		if json.Unmarshal([]byte(raw), &policy) != nil || ValidateDeploymentPolicy(policy) != nil {
			policy = DefaultDeploymentPolicy()
		}
	}
	window := policy.PendingTimeout()
	last, found, err := t.store.LatestScheduleEvent(deployment)
	t.check(err)
	if err != nil {
		return 0, err
	}
	if found {
		if since := time.Since(last.CreatedAt); since < window {
			return window - since, nil
		}
	}
	data, err := json.Marshal(map[string]any{"scheduler": reason})
	if err != nil {
		return 0, err
	}
	if t.err != nil {
		return 0, t.err
	}
	err = t.store.db.Create(&InstanceEvent{DeploymentID: deployment, Kind: EventKindSchedule, Code: EventDeploymentUnschedulable, Data: string(data), CreatedAt: time.Now().UTC()}).Error
	if err != nil {
		t.err = err
	}
	return window, err
}
