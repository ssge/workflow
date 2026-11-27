package db

import (
	"errors"
	"fmt"
	"math"

	"github.com/ssge/workflow/db/store"
	"gorm.io/gorm/clause"
)

func (tx *runtimeTx) headForUpdate(key string) (DefinitionHead, error) {
	var head DefinitionHead
	err := tx.lock(tx.db, "").Take(&head, "process_key = ?", key).Error
	return head, storeError(err)
}

func (tx *runtimeTx) Publish(in store.Publication) (store.DefinitionRef, error) {
	row := Definition{
		ProcessKey: in.ProcessKey, Version: 1, ProfileRevision: in.ProfileRevision,
		Sources: JSONArray(in.Sources), Plan: JSONObject(in.Plan), CreatedAt: in.CreatedAt,
	}
	head, err := tx.headForUpdate(in.ProcessKey)
	if errors.Is(err, store.ErrNotFound) {
		// There is no head row to lock for the first publication. The
		// version-1 constraint arbitrates that race without aborting the
		// losing PostgreSQL transaction.
		q := tx.db.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "process_key"}, {Name: "version"}}, DoNothing: true,
		}).Create(&row)
		if err := tx.write(q); err != nil {
			return store.DefinitionRef{}, err
		}
		if q.RowsAffected == 1 {
			err := tx.write(tx.db.Create(&DefinitionHead{ProcessKey: row.ProcessKey, Version: row.Version}))
			if err != nil {
				return store.DefinitionRef{}, err
			}
			return store.DefinitionRef{ProcessKey: row.ProcessKey, Version: row.Version}, nil
		}
		head, err = tx.headForUpdate(in.ProcessKey)
		if errors.Is(err, store.ErrNotFound) {
			return store.DefinitionRef{}, store.ErrConflict
		}
	}
	if err != nil {
		return store.DefinitionRef{}, err
	}
	if head.Version == math.MaxInt64 {
		return store.DefinitionRef{}, fmt.Errorf("process %q exhausted its versions", in.ProcessKey)
	}
	row.Version = head.Version + 1
	if err := tx.write(tx.db.Create(&row)); err != nil {
		return store.DefinitionRef{}, err
	}
	q := tx.db.Model(&DefinitionHead{}).Where("process_key = ?", row.ProcessKey).Update("version", row.Version)
	if err := tx.changed(q, store.ErrConflict); err != nil {
		return store.DefinitionRef{}, err
	}
	return store.DefinitionRef{ProcessKey: row.ProcessKey, Version: row.Version}, nil
}

func (tx *runtimeTx) GetDefinition(ref store.DefinitionRef) (store.Definition, error) {
	var row Definition
	err := tx.db.Take(&row, "process_key = ? AND version = ?", ref.ProcessKey, ref.Version).Error
	return store.Definition{
		Ref: ref, ProfileRevision: row.ProfileRevision,
		Sources: store.Array(row.Sources), Plan: store.Object(row.Plan), CreatedAt: row.CreatedAt,
	}, storeError(err)
}

func (tx *runtimeTx) ListDefinitions(key string, afterVersion int64, limit int) ([]store.DefinitionVersion, error) {
	if afterVersion < 0 || limit <= 0 {
		return nil, fmt.Errorf("%w: expected nonnegative afterVersion and positive limit", store.ErrInvalidArgument)
	}
	var rows []Definition
	err := tx.db.Select("process_key", "version", "created_at").
		Where("process_key = ? AND version > ?", key, afterVersion).
		Order("version ASC").Limit(limit).Find(&rows).Error
	if err != nil {
		return nil, storeError(err)
	}
	versions := make([]store.DefinitionVersion, 0, len(rows))
	for _, row := range rows {
		versions = append(versions, store.DefinitionVersion{
			Ref: store.DefinitionRef{ProcessKey: row.ProcessKey, Version: row.Version}, CreatedAt: row.CreatedAt,
		})
	}
	return versions, nil
}

func (tx *runtimeTx) GetHead(key string) (store.DefinitionRef, error) {
	var row DefinitionHead
	if err := tx.db.Take(&row, "process_key = ?", key).Error; err != nil {
		return store.DefinitionRef{}, storeError(err)
	}
	return store.DefinitionRef{ProcessKey: row.ProcessKey, Version: row.Version}, nil
}

func (tx *runtimeTx) ListInstancePins(ref store.DefinitionRef) ([]store.InstancePin, error) {
	var pins []store.InstancePin
	err := tx.db.Model(&Instance{}).Select("instance_id", "status").
		Where("process_key = ? AND version = ?", ref.ProcessKey, ref.Version).
		Order("instance_id ASC").Find(&pins).Error
	if err != nil {
		return nil, storeError(err)
	}
	return pins, nil
}

func (tx *runtimeTx) CountInstancePins(ref store.DefinitionRef, statuses ...store.InstanceStatus) (int64, error) {
	q := tx.db.Model(&Instance{}).Where("process_key = ? AND version = ?", ref.ProcessKey, ref.Version)
	if len(statuses) != 0 {
		q = q.Where("status IN ?", statuses)
	}
	var count int64
	err := q.Count(&count).Error
	return count, storeError(err)
}

func (tx *runtimeTx) CreateInstanceAtHead(in store.InstanceStart) (store.Instance, error) {
	head, err := tx.headForUpdate(in.ProcessKey)
	if err != nil {
		return store.Instance{}, err
	}
	row := Instance{
		InstanceID: in.ID, ProcessKey: head.ProcessKey, Version: head.Version,
		Status: string(store.InstanceRunning), Revision: 1, Variables: JSONObject(in.Variables), CreatedAt: in.CreatedAt,
	}
	if err := tx.write(tx.db.Create(&row)); err != nil {
		return store.Instance{}, err
	}
	if err := tx.insertTransition(row.InstanceID, 1, store.JournalEntry{Kind: store.TransitionStarted, AtMS: in.CreatedAt}); err != nil {
		return store.Instance{}, err
	}
	return instanceValue(row), nil
}

func (tx *runtimeTx) GetInstance(id string) (store.Instance, error) {
	var row Instance
	err := tx.db.Take(&row, "instance_id = ?", id).Error
	return instanceValue(row), storeError(err)
}

func instanceValue(row Instance) store.Instance {
	return store.Instance{
		ID: row.InstanceID, Ref: store.DefinitionRef{ProcessKey: row.ProcessKey, Version: row.Version},
		Status: store.InstanceStatus(row.Status), Revision: row.Revision, Variables: store.Object(row.Variables),
		CreatedAt: row.CreatedAt, EndedAt: row.EndedAt,
	}
}

func (tx *runtimeTx) ChangeInstance(in store.InstanceChange) error {
	if in.ExpectedRevision <= 0 || in.ExpectedRevision == math.MaxInt64 {
		return fmt.Errorf("%w: instance revision cannot be incremented", store.ErrInvalidArgument)
	}
	next := in.ExpectedRevision + 1
	q := tx.db.Model(&Instance{}).
		Where("instance_id = ? AND revision = ? AND status <> ?", in.ID, in.ExpectedRevision, store.InstanceCompleted).
		Updates(map[string]any{
			"revision": next, "status": in.Status, "variables": JSONObject(in.Variables), "ended_at": in.EndedAt,
		})
	if err := tx.changed(q, store.ErrConflict); err != nil {
		return err
	}
	return tx.insertTransition(in.ID, next, in.Entry)
}

func (tx *runtimeTx) insertTransition(id string, revision int64, entry store.JournalEntry) error {
	return tx.write(tx.db.Create(&Transition{
		InstanceID: id, Revision: revision, Kind: string(entry.Kind), ElementID: entry.ElementID, AtMS: entry.AtMS,
	}))
}

func (tx *runtimeTx) GetTransition(id string, revision int64) (store.Transition, error) {
	var row Transition
	err := tx.db.Take(&row, "instance_id = ? AND revision = ?", id, revision).Error
	return store.Transition{
		InstanceID: row.InstanceID, Revision: row.Revision,
		JournalEntry: store.JournalEntry{Kind: store.TransitionKind(row.Kind), ElementID: row.ElementID, AtMS: row.AtMS},
	}, storeError(err)
}

func (tx *runtimeTx) GetActivation(id string) (store.Activation, error) {
	var row Activation
	err := tx.db.Take(&row, "activation_id = ?", id).Error
	return store.Activation{
		ID: row.ActivationID, InstanceID: row.InstanceID, TokenID: row.TokenID, ElementID: row.ElementID,
		State: store.ActivationState(row.State),
	}, storeError(err)
}

func (tx *runtimeTx) InsertActivation(in store.Activation) error {
	return tx.write(tx.db.Create(&Activation{
		ActivationID: in.ID, InstanceID: in.InstanceID, TokenID: in.TokenID, ElementID: in.ElementID, State: string(in.State),
	}))
}

func (tx *runtimeTx) SetActivationState(id string, expected, next store.ActivationState) error {
	q := tx.db.Model(&Activation{}).
		Where("activation_id = ? AND state = ? AND state <> ?", id, expected, store.ActivationConsumed).
		Update("state", next)
	return tx.changed(q, store.ErrConflict)
}

func (tx *runtimeTx) EnqueueStep(in store.StepWork) error {
	return tx.write(tx.db.Create(&WorkItem{
		WorkID: in.ID, Kind: string(store.WorkStep), InstanceID: &in.InstanceID, ActivationID: &in.ActivationID, ReadyAt: in.ReadyAt,
	}))
}
