package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/ssge/workflow/db/store"
	"github.com/ssge/workflow/internal/application"
)

func (e *Engine) GetOperation(ctx context.Context, id string) (application.Operation, error) {
	var op store.Operation
	err := e.store.WithTransaction(ctx, func(tx store.Tx) error {
		var err error
		op, err = tx.GetOperation(id)
		return err
	})
	if err != nil {
		return application.Operation{}, readError(err, "operation not found")
	}
	out := application.Operation{OperationID: op.ID, Status: string(op.Status)}
	if op.Result != nil {
		out.Result = json.RawMessage(*op.Result)
	}
	if op.Error != nil {
		var failure application.Failure
		d := json.NewDecoder(strings.NewReader(string(*op.Error)))
		d.UseNumber()
		if err := d.Decode(&failure); err != nil {
			return application.Operation{}, unavailable()
		}
		out.Error = &failure
	}
	return out, nil
}

func (e *Engine) GetOperationByKey(ctx context.Context, key string) (application.Operation, error) {
	if !validKey(key) {
		return application.Operation{}, serviceError(400, "INVALID_REQUEST", "invalid idempotency key")
	}
	return e.GetOperation(ctx, operationID(key))
}

func (e *Engine) ListDefinitionVersions(ctx context.Context, key string, limit int, cursor string) (application.Page[application.DefinitionVersion], error) {
	page := application.Page[application.DefinitionVersion]{Items: []application.DefinitionVersion{}}
	if limit == 0 {
		limit = 50
	}
	after, valid := versionAfter(key, cursor)
	if key == "" || limit < 1 || limit > 200 || !valid {
		return page, serviceError(400, "INVALID_REQUEST", "invalid definition list parameters")
	}
	err := e.store.WithTransaction(ctx, func(tx store.Tx) error {
		versions, err := tx.ListDefinitions(key, after, limit+1)
		if err != nil || len(versions) == 0 {
			return err
		}
		// Read the head after the version page: at READ COMMITTED, a head
		// read first could predate a version returned by the subsequent list.
		head, err := tx.GetHead(key)
		if err != nil {
			return err
		}
		if len(versions) > limit {
			versions = versions[:limit]
			next := versionCursor(versions[limit-1].Ref)
			page.NextCursor = &next
		}
		for _, version := range versions {
			label := "CURRENT"
			if version.Ref != head {
				count, err := tx.CountInstancePins(version.Ref, store.InstanceRunning, store.InstanceIncident)
				if err != nil {
					return err
				}
				label = "DRAINED"
				if count != 0 {
					label = "DRAINING"
				}
			}
			page.Items = append(page.Items, application.DefinitionVersion{
				ProcessKey: version.Ref.ProcessKey, Version: version.Ref.Version,
				Label: label, CreatedAt: time.UnixMilli(version.CreatedAt).UTC(),
			})
		}
		return nil
	})
	if err != nil {
		return application.Page[application.DefinitionVersion]{}, unavailable()
	}
	return page, nil
}

func versionCursor(ref store.DefinitionRef) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(ref.Version, 10) + ":" + ref.ProcessKey))
}

func versionAfter(key, cursor string) (int64, bool) {
	if cursor == "" {
		return 0, true
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, false
	}
	value, cursorKey, _ := strings.Cut(string(raw), ":")
	after, err := strconv.ParseInt(value, 10, 64)
	ref := store.DefinitionRef{Version: after, ProcessKey: key}
	return after, err == nil && after > 0 && cursorKey == key && cursor == versionCursor(ref)
}

func (e *Engine) GetInstance(ctx context.Context, id string) (application.Instance, error) {
	var in store.Instance
	var incident *store.Incident
	err := e.store.WithTransaction(ctx, func(tx store.Tx) error {
		var err error
		in, err = tx.GetInstance(id)
		if err != nil {
			return err
		}
		if in.Status == store.InstanceIncident {
			value, err := tx.GetIncident(in.ID, in.Revision)
			if err != nil {
				return err
			}
			incident = &value
		}
		return err
	})
	if err != nil {
		return application.Instance{}, readError(err, "instance not found")
	}
	out := application.Instance{
		InstanceID: in.ID, ProcessKey: in.Ref.ProcessKey, Version: in.Ref.Version,
		Status: string(in.Status), Revision: in.Revision, Variables: json.RawMessage(in.Variables),
		CreatedAt: time.UnixMilli(in.CreatedAt).UTC(),
	}
	if in.EndedAt != nil {
		endedAt := time.UnixMilli(*in.EndedAt).UTC()
		out.EndedAt = &endedAt
	}
	if incident != nil {
		out.Incident = &application.Incident{
			Code: incident.Code, ElementID: incident.ElementID, Message: incident.Message,
		}
	}
	return out, nil
}

type taskCursor struct {
	InstanceID string `json:"instanceId"`
	Status     string `json:"status"`
	CreatedAt  int64  `json:"createdAt"`
	TaskID     string `json:"taskId"`
}

func (e *Engine) ListTasks(ctx context.Context, instanceID, status string, limit int, cursor string) (application.Page[application.TaskSummary], error) {
	page := application.Page[application.TaskSummary]{Items: []application.TaskSummary{}}
	if limit == 0 {
		limit = 50
	}
	if status == "" {
		status = string(store.TaskOpen)
	}
	if limit < 1 || limit > 200 ||
		(status != string(store.TaskOpen) && status != string(store.TaskCompleted) && status != string(store.TaskCancelled)) {
		return page, serviceError(400, "INVALID_REQUEST", "invalid task list parameters")
	}
	var after *int64
	var afterID string
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			return page, serviceError(400, "INVALID_REQUEST", "invalid task cursor")
		}
		var decoded taskCursor
		if err := json.Unmarshal(raw, &decoded); err != nil || decoded.InstanceID != instanceID ||
			decoded.Status != status || decoded.TaskID == "" {
			return page, serviceError(400, "INVALID_REQUEST", "task cursor does not match filters")
		}
		canonical, _ := json.Marshal(decoded)
		if cursor != base64.RawURLEncoding.EncodeToString(canonical) {
			return page, serviceError(400, "INVALID_REQUEST", "invalid task cursor")
		}
		after, afterID = &decoded.CreatedAt, decoded.TaskID
	}
	var records []store.TaskRecord
	err := e.store.WithTransaction(ctx, func(tx store.Tx) error {
		var err error
		records, err = tx.ListUserTasks(store.TaskQuery{
			InstanceID: instanceID, Status: status, AfterCreatedAt: after, AfterID: afterID, Limit: limit + 1,
		})
		return err
	})
	if err != nil {
		return application.Page[application.TaskSummary]{}, unavailable()
	}
	if len(records) > limit {
		records = records[:limit]
		last := records[len(records)-1].Task
		raw, _ := json.Marshal(taskCursor{InstanceID: instanceID, Status: status, CreatedAt: last.CreatedAt, TaskID: last.ID})
		next := base64.RawURLEncoding.EncodeToString(raw)
		page.NextCursor = &next
	}
	for _, record := range records {
		task := record.Task
		summary := application.TaskSummary{
			TaskID: task.ID, InstanceID: record.Instance.ID, ElementID: record.ElementID,
			Version: record.Instance.Ref.Version, Status: string(task.Status),
			CreatedAt: time.UnixMilli(task.CreatedAt).UTC(),
		}
		if record.DueAt != nil {
			due := time.UnixMilli(*record.DueAt).UTC()
			summary.DueAt = &due
		}
		page.Items = append(page.Items, summary)
	}
	return page, nil
}

func (e *Engine) GetTask(ctx context.Context, id string) (application.Task, error) {
	var task store.UserTask
	var activation store.Activation
	var instance store.Instance
	var timer *store.BoundaryTimer
	err := e.store.WithTransaction(ctx, func(tx store.Tx) error {
		var err error
		task, err = tx.GetUserTask(id)
		if err != nil {
			return err
		}
		activation, err = tx.GetActivation(task.ActivationID)
		if err != nil {
			return err
		}
		instance, err = tx.GetInstance(activation.InstanceID)
		if err != nil {
			return err
		}
		value, err := tx.GetBoundaryTimer(activation.ID)
		if err == nil {
			timer = &value
			return nil
		}
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	})
	if err != nil {
		return application.Task{}, readError(err, "task not found")
	}
	out := application.Task{
		TaskSummary: application.TaskSummary{
			TaskID: task.ID, InstanceID: instance.ID, ElementID: activation.ElementID,
			Version: instance.Ref.Version, Status: string(task.Status),
			CreatedAt: time.UnixMilli(task.CreatedAt).UTC(),
		},
		FormID: task.FormID, Components: json.RawMessage(task.Components),
	}
	if timer != nil {
		due := time.UnixMilli(timer.DueAt).UTC()
		out.DueAt = &due
	}
	if task.ClosedAt != nil {
		closed := time.UnixMilli(*task.ClosedAt).UTC()
		out.ClosedAt = &closed
	}
	if task.Reason != nil {
		reason := string(*task.Reason)
		out.Reason = &reason
	}
	return out, nil
}

func readError(err error, missing string) error {
	if errors.Is(err, store.ErrNotFound) {
		return serviceError(404, "NOT_FOUND", missing)
	}
	return unavailable()
}
