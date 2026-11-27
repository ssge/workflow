package db

import (
	"context"
	"fmt"

	"github.com/ssge/workflow/db/store"
	"gorm.io/gorm/clause"
)

func (tx *runtimeTx) insertUnique(row any) error {
	return tx.changed(tx.db.Clauses(clause.OnConflict{DoNothing: true}).Create(row), store.ErrConflict)
}

func (tx *runtimeTx) InsertUserTask(in store.NewUserTask) error {
	return tx.insertUnique(&UserTask{
		TaskID: in.ID, ActivationID: in.ActivationID, FormID: in.FormID,
		Components: JSONArray(in.Components), CreatedAt: in.CreatedAt,
	})
}

func (tx *runtimeTx) GetUserTask(id string) (store.UserTask, error) {
	var row UserTask
	err := tx.db.Take(&row, "task_id = ?", id).Error
	return userTaskValue(row), storeError(err)
}

func (tx *runtimeTx) GetUserTaskByActivation(id string) (store.UserTask, error) {
	var row UserTask
	err := tx.db.Take(&row, "activation_id = ?", id).Error
	return userTaskValue(row), storeError(err)
}

func (tx *runtimeTx) ListUserTasks(query store.TaskQuery) ([]store.TaskRecord, error) {
	if query.Limit <= 0 || query.Status == "" {
		return nil, fmt.Errorf("%w: task query requires status and positive limit", store.ErrInvalidArgument)
	}
	q := tx.db.Table("user_tasks AS tasks").
		Select("tasks.*, activations.element_id, instances.instance_id, instances.process_key, instances.version, instances.status AS instance_status, instances.revision, instances.variables, instances.created_at AS instance_created_at, instances.ended_at, timers.due_at").
		Joins("JOIN activations ON activations.activation_id = tasks.activation_id").
		Joins("JOIN instances ON instances.instance_id = activations.instance_id").
		Joins("LEFT JOIN boundary_timers AS timers ON timers.activation_id = tasks.activation_id").
		Where("tasks.status = ?", query.Status)
	if query.InstanceID != "" {
		q = q.Where("instances.instance_id = ?", query.InstanceID)
	}
	if query.AfterCreatedAt != nil {
		q = q.Where("(tasks.created_at > ?) OR (tasks.created_at = ? AND tasks.task_id > ?)", *query.AfterCreatedAt, *query.AfterCreatedAt, query.AfterID)
	}
	type row struct {
		UserTask
		ElementID         string
		InstanceID        string
		ProcessKey        string
		Version           int64
		InstanceStatus    string
		Revision          int64
		Variables         JSONObject
		InstanceCreatedAt int64
		EndedAt           *int64
		DueAt             *int64
	}
	var rows []row
	if err := q.Order("tasks.created_at ASC, tasks.task_id ASC").Limit(query.Limit).Scan(&rows).Error; err != nil {
		return nil, storeError(err)
	}
	out := make([]store.TaskRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.TaskRecord{
			Task: userTaskValue(row.UserTask),
			Instance: store.Instance{
				ID: row.InstanceID, Ref: store.DefinitionRef{ProcessKey: row.ProcessKey, Version: row.Version},
				Status: store.InstanceStatus(row.InstanceStatus), Revision: row.Revision, Variables: store.Object(row.Variables),
				CreatedAt: row.InstanceCreatedAt, EndedAt: row.EndedAt,
			},
			ElementID: row.ElementID, DueAt: row.DueAt,
		})
	}
	return out, nil
}

func userTaskValue(row UserTask) store.UserTask {
	return store.UserTask{
		NewUserTask: store.NewUserTask{ID: row.TaskID, ActivationID: row.ActivationID, FormID: row.FormID,
			Components: store.Array(row.Components), CreatedAt: row.CreatedAt},
		Status: store.TaskStatus(row.Status), ClosedAt: row.ClosedAt, Reason: (*store.TaskCloseReason)(row.Reason),
	}
}

func (tx *runtimeTx) CloseUserTask(id string, in store.TaskClosure) error {
	completed := in.Status == store.TaskCompleted && in.Reason == nil
	cancelled := in.Status == store.TaskCancelled && in.Reason != nil && *in.Reason == store.TaskCancelledByTimer
	if !completed && !cancelled {
		return fmt.Errorf("%w: invalid task closure", store.ErrInvalidArgument)
	}
	q := tx.db.Model(&UserTask{}).Where("task_id = ? AND status = ?", id, store.TaskOpen).
		Updates(map[string]any{"status": in.Status, "closed_at": in.ClosedAt, "reason": (*string)(in.Reason)})
	return tx.changed(q, store.ErrConflict)
}

func (tx *runtimeTx) InsertBoundaryTimer(in store.NewBoundaryTimer) error {
	return tx.insertUnique(&BoundaryTimer{
		TimerID: in.ID, ActivationID: in.ActivationID, ElementID: in.ElementID, DueAt: in.DueAt,
	})
}

func (tx *runtimeTx) GetBoundaryTimer(id string) (store.BoundaryTimer, error) {
	var row BoundaryTimer
	err := tx.db.Take(&row, "activation_id = ?", id).Error
	return store.BoundaryTimer{
		NewBoundaryTimer: store.NewBoundaryTimer{
			ID: row.TimerID, ActivationID: row.ActivationID, ElementID: row.ElementID, DueAt: row.DueAt,
		},
		State: store.TimerState(row.State),
	}, storeError(err)
}

func (tx *runtimeTx) TransitionBoundaryTimer(id string, next store.TimerState) error {
	if next != store.TimerFired && next != store.TimerCancelled {
		return fmt.Errorf("%w: invalid terminal timer state", store.ErrInvalidArgument)
	}
	q := tx.db.Model(&BoundaryTimer{}).Where("activation_id = ? AND state = ?", id, store.TimerArmed).
		Update("state", next)
	return tx.changed(q, store.ErrConflict)
}

func (tx *runtimeTx) EnqueueTimerWork(workID, activationID string) error {
	var timer BoundaryTimer
	if err := tx.lock(tx.db, "").Take(&timer, "activation_id = ?", activationID).Error; err != nil {
		return storeError(err)
	}
	if store.TimerState(timer.State) != store.TimerArmed {
		return store.ErrConflict
	}
	activation, err := tx.GetActivation(activationID)
	if err != nil {
		return err
	}
	return tx.insertUnique(&WorkItem{
		WorkID: workID, Kind: string(store.WorkTimer), InstanceID: &activation.InstanceID,
		ActivationID: &activationID, ReadyAt: timer.DueAt,
	})
}

func (tx *runtimeTx) CreateFork(in store.NewFork) error {
	return tx.insertUnique(&Fork{
		ForkID: in.ID, InstanceID: in.InstanceID, SplitActivationID: in.SplitActivationID, JoinElementID: in.JoinElementID,
	})
}

func (tx *runtimeTx) GetFork(id string) (store.Fork, error) {
	var row Fork
	err := tx.db.Take(&row, "fork_id = ?", id).Error
	return store.Fork{
		NewFork: store.NewFork{ID: row.ForkID, InstanceID: row.InstanceID, SplitActivationID: row.SplitActivationID, JoinElementID: row.JoinElementID},
		State:   store.ForkState(row.State),
	}, storeError(err)
}

func (tx *runtimeTx) lockOpenFork(id string) error {
	var row Fork
	if err := tx.lock(tx.db, "").Select("fork_id", "state").Take(&row, "fork_id = ?", id).Error; err != nil {
		return storeError(err)
	}
	if store.ForkState(row.State) != store.ForkOpen {
		return store.ErrConflict
	}
	return nil
}

func (tx *runtimeTx) InsertBranchActivation(in store.BranchActivation) error {
	if err := tx.lockOpenFork(in.ForkID); err != nil {
		return err
	}
	return tx.insertUnique(&BranchActivation{
		ActivationID: in.ActivationID, InstanceID: in.InstanceID, ForkID: in.ForkID, BranchFlowID: in.BranchFlowID,
	})
}

func (tx *runtimeTx) GetBranchActivation(id string) (store.BranchActivation, error) {
	var out store.BranchActivation
	err := tx.db.Model(&BranchActivation{}).Take(&out, "activation_id = ?", id).Error
	return out, storeError(err)
}

func (tx *runtimeTx) InsertJoinArrival(in store.JoinArrival) error {
	if err := tx.lockOpenFork(in.ForkID); err != nil {
		return err
	}
	return tx.insertUnique(&JoinArrival{
		ForkID: in.ForkID, IncomingFlowID: in.IncomingFlowID, BranchFlowID: in.BranchFlowID, ActivationID: in.ActivationID,
	})
}

func (tx *runtimeTx) ListJoinArrivals(forkID string) ([]store.JoinArrival, error) {
	var out []store.JoinArrival
	err := tx.db.Model(&JoinArrival{}).Where("fork_id = ?", forkID).Order("incoming_flow_id ASC").Find(&out).Error
	return out, storeError(err)
}

func (tx *runtimeTx) CloseFork(id string) error {
	q := tx.db.Model(&Fork{}).Where("fork_id = ? AND state = ?", id, store.ForkOpen).
		Update("state", store.ForkClosed)
	return tx.changed(q, store.ErrConflict)
}

func (s *runtimeStore) RaiseIncident(ctx context.Context, lease store.Lease, in store.IncidentRaise) error {
	return s.applyLeased(ctx, lease, "BLOCKED", func(tx *runtimeTx, work WorkItem) error {
		if work.InstanceID == nil {
			return fmt.Errorf("%w: incident requires instance work", store.ErrInvalidArgument)
		}
		instance, err := tx.GetInstance(*work.InstanceID)
		if err != nil {
			return err
		}
		if instance.Revision != in.ExpectedRevision {
			return store.ErrConflict
		}
		if err := tx.ChangeInstance(store.InstanceChange{
			ID: instance.ID, ExpectedRevision: in.ExpectedRevision, Status: store.InstanceIncident, Variables: instance.Variables,
			Entry: store.JournalEntry{Kind: store.TransitionIncidentRaised, ElementID: &in.ElementID, AtMS: in.AtMS},
		}); err != nil {
			return err
		}
		return tx.write(tx.db.Create(&Incident{
			InstanceID: instance.ID, Revision: in.ExpectedRevision + 1, WorkID: work.WorkID,
			ElementID: in.ElementID, Code: in.Code, Message: in.Message, RetryInput: JSONObject(in.RetryInput),
		}))
	})
}

func (tx *runtimeTx) GetIncident(instanceID string, revision int64) (store.Incident, error) {
	var row Incident
	err := tx.db.Take(&row, "instance_id = ? AND revision = ?", instanceID, revision).Error
	return store.Incident{
		InstanceID: row.InstanceID, Revision: row.Revision, WorkID: row.WorkID, ElementID: row.ElementID,
		Code: row.Code, Message: row.Message, RetryInput: store.Object(row.RetryInput),
	}, storeError(err)
}
