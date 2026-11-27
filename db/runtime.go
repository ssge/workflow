package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"github.com/mattn/go-sqlite3"
	"github.com/ssge/workflow/db/store"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type runtimeStore struct {
	db    *gorm.DB
	clock store.Clock
}

// NewRuntimeStore binds the runtime port to a database configured by Open
// and migrated before use. The supplied clock must not be nil.
func NewRuntimeStore(database *gorm.DB, clock store.Clock) store.Store {
	return &runtimeStore{db: database, clock: clock}
}

type runtimeTx struct {
	db       *gorm.DB
	writeErr error
}

func (s *runtimeStore) transaction(ctx context.Context, fn func(*runtimeTx) error) error {
	var opts *sql.TxOptions
	if s.db.Dialector.Name() == "postgres" {
		// In particular, INSERT ON CONFLICT followed by a read must see the
		// concurrent winner's committed row.
		opts = &sql.TxOptions{Isolation: sql.LevelReadCommitted}
	}
	return storeError(s.db.WithContext(ctx).Transaction(func(db *gorm.DB) error {
		tx := &runtimeTx{db: db}
		if err := fn(tx); err != nil {
			return err
		}
		return tx.writeErr
	}, opts))
}

func (s *runtimeStore) WithTransaction(ctx context.Context, fn func(store.Tx) error) error {
	return s.transaction(ctx, func(tx *runtimeTx) error { return fn(tx) })
}

// SQLite's BEGIN IMMEDIATE (configured by Open) already serializes writers.
func (tx *runtimeTx) lock(q *gorm.DB, options string) *gorm.DB {
	if tx.db.Dialector.Name() == "postgres" {
		return q.Clauses(clause.Locking{Strength: "UPDATE", Options: options})
	}
	return q
}

func (tx *runtimeTx) write(q *gorm.DB) error {
	err := storeError(q.Error)
	if err != nil && tx.writeErr == nil {
		// SQLite can keep the transaction open after a statement failure.
		// Do not commit partial effects even if a callback ignores the error.
		tx.writeErr = err
	}
	return err
}

func (tx *runtimeTx) changed(q *gorm.DB, conflict error) error {
	if err := tx.write(q); err != nil {
		return err
	}
	if q.RowsAffected != 1 {
		return conflict
	}
	return nil
}

func storeError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, gorm.ErrRecordNotFound):
		return store.ErrNotFound
	case errors.Is(err, store.ErrConflict):
		return err
	}
	var se sqlite3.Error
	if errors.As(err, &se) && (se.Code == sqlite3.ErrBusy || se.Code == sqlite3.ErrLocked) {
		return fmt.Errorf("%w: %w", store.ErrConflict, err)
	}
	var pg interface{ SQLState() string }
	if errors.As(err, &pg) {
		switch pg.SQLState() {
		case "40001", "40P01", "55P03": // serialization, deadlock, lock unavailable
			return fmt.Errorf("%w: %w", store.ErrConflict, err)
		}
	}
	return err
}

func (s *runtimeStore) Admit(ctx context.Context, in store.Admission) (store.Operation, error) {
	if in.OperationID == "" || in.WorkID == "" {
		return store.Operation{}, fmt.Errorf("%w: admission requires operation and work IDs", store.ErrInvalidArgument)
	}
	var out store.Operation
	err := s.transaction(ctx, func(tx *runtimeTx) error {
		row := Operation{
			OperationID: in.OperationID, IdempotencyKey: CommandKey(in.Key), Kind: string(in.Command.Kind),
			Target: JSONObject(in.Command.Target), Payload: JSONObject(in.Command.Payload), Status: string(store.OperationPending),
		}
		insert := tx.db.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "idempotency_key"}}, DoNothing: true,
		}).Create(&row)
		if err := tx.write(insert); err != nil {
			return err
		}
		if insert.RowsAffected == 1 {
			if err := tx.write(tx.db.Create(&WorkItem{
				WorkID: in.WorkID, Kind: string(store.WorkCommand), OperationID: &row.OperationID, ReadyAt: in.ReadyAt,
			})); err != nil {
				return err
			}
		} else {
			row = Operation{}
			if err := tx.db.Take(&row, "idempotency_key = ?", in.Key).Error; err != nil {
				return err
			}
			equal, err := sameCommand(operationValue(row).Command, in.Command)
			if err != nil {
				return err
			}
			if !equal {
				return store.ErrIdempotencyConflict
			}
		}
		out = operationValue(row)
		return nil
	})
	if err != nil {
		return store.Operation{}, err
	}
	return out, nil
}

func (tx *runtimeTx) GetOperation(id string) (store.Operation, error) {
	var row Operation
	err := tx.db.Take(&row, "operation_id = ?", id).Error
	return operationValue(row), storeError(err)
}

func operationValue(row Operation) store.Operation {
	return store.Operation{
		ID: row.OperationID, Status: store.OperationStatus(row.Status),
		Command: store.Command{Kind: store.CommandKind(row.Kind), Target: store.Object(row.Target), Payload: store.Object(row.Payload)},
		Outcome: store.Outcome{Result: (*store.Object)(row.Result), Error: (*store.Object)(row.Error)},
	}
}

func (tx *runtimeTx) finishOperation(id string, outcome *store.Outcome) error {
	if outcome == nil || (outcome.Result == nil) == (outcome.Error == nil) {
		return fmt.Errorf("%w: command requires one terminal outcome", store.ErrInvalidArgument)
	}
	status := store.OperationSucceeded
	if outcome.Error != nil {
		status = store.OperationFailed
	}
	return tx.changed(tx.db.Model(&Operation{}).
		Where("operation_id = ? AND status = ?", id, store.OperationPending).
		Updates(map[string]any{
			"status": status, "result": (*JSONObject)(outcome.Result), "error": (*JSONObject)(outcome.Error),
		}), store.ErrConflict)
}

func eligible(q *gorm.DB, now int64) *gorm.DB {
	return q.Where("ready_at <= ? AND (state = 'READY' OR (state = 'LEASED' AND lease_until <= ?))", now, now)
}

func (s *runtimeStore) Claim(ctx context.Context, in store.ClaimRequest) ([]store.Claim, error) {
	if in.Owner == "" || in.Limit <= 0 || in.LeaseMS <= 0 ||
		(in.Kind != store.WorkCommand && in.Kind != store.WorkStep && in.Kind != store.WorkTimer) {
		return nil, fmt.Errorf("%w: invalid claim request", store.ErrInvalidArgument)
	}
	var claims []store.Claim
	err := s.transaction(ctx, func(tx *runtimeTx) error {
		var rows []WorkItem
		base := tx.db.Where("kind = ?", in.Kind)
		if in.Kind != store.WorkCommand {
			base = base.Where("EXISTS (SELECT 1 FROM instances WHERE instances.instance_id = work_items.instance_id AND instances.status <> ?)", store.InstanceIncident)
		}
		query := eligible(base, s.clock())
		if err := tx.lock(query, "SKIP LOCKED").Order("ready_at, work_id").Limit(in.Limit).Find(&rows).Error; err != nil {
			return err
		}
		// Start the lease after obtaining the candidate locks, not before a
		// potentially contended begin or scan.
		now := s.clock()
		if now > math.MaxInt64-in.LeaseMS {
			return fmt.Errorf("%w: lease expiry overflows", store.ErrInvalidArgument)
		}
		expires := now + in.LeaseMS
		for _, row := range rows {
			if row.Fence == math.MaxInt64 {
				return fmt.Errorf("work %q exhausted its fencing tokens", row.WorkID)
			}
			fence := row.Fence + 1
			q := eligible(tx.db.Model(&WorkItem{}).Where("work_id = ? AND fence = ?", row.WorkID, row.Fence), now)
			if err := tx.changed(q.Updates(map[string]any{
				"state": "LEASED", "owner": in.Owner, "fence": fence, "lease_until": expires,
			}), store.ErrConflict); err != nil {
				return err
			}
			claims = append(claims, store.Claim{
				Work:  workValue(row),
				Lease: store.Lease{WorkID: row.WorkID, Owner: in.Owner, Fence: fence, ExpiresAt: expires},
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claims, nil
}

func leaseMatch(db *gorm.DB, lease store.Lease) *gorm.DB {
	return db.Where("work_id = ? AND state = 'LEASED' AND owner = ? AND fence = ?", lease.WorkID, lease.Owner, lease.Fence)
}

func (s *runtimeStore) Apply(ctx context.Context, lease store.Lease, fn func(store.Tx, store.WorkItem) (*store.Outcome, error)) error {
	return s.applyLeased(ctx, lease, "DONE", func(tx *runtimeTx, row WorkItem) error {
		outcome, err := fn(tx, workValue(row))
		if err != nil {
			return err
		}
		if tx.writeErr != nil {
			return tx.writeErr
		}
		if row.Kind == string(store.WorkCommand) {
			return tx.finishOperation(*row.OperationID, outcome)
		}
		if outcome != nil {
			return fmt.Errorf("%w: only command work has an operation outcome", store.ErrInvalidArgument)
		}
		return nil
	})
}

func (s *runtimeStore) applyLeased(ctx context.Context, lease store.Lease, next string, fn func(*runtimeTx, WorkItem) error) error {
	return s.transaction(ctx, func(tx *runtimeTx) error {
		var row WorkItem
		if err := readLease(leaseMatch(tx.db, lease), &row); err != nil {
			return err
		}
		if *row.LeaseUntil <= s.clock() {
			return store.ErrLeaseLost
		}
		if err := fn(tx, row); err != nil {
			return err
		}
		// Take the row lock before sampling the final clock, so time spent
		// waiting behind a concurrent writer cannot extend the old lease.
		var locked WorkItem
		if err := readLease(tx.lock(leaseMatch(tx.db, lease).Select("work_id"), ""), &locked); err != nil {
			return err
		}
		q := leaseMatch(tx.db.Model(&WorkItem{}), lease).Where("lease_until > ?", s.clock())
		return tx.changed(q.Updates(map[string]any{"state": next, "owner": nil, "lease_until": nil}), store.ErrLeaseLost)
	})
}

func readLease(query *gorm.DB, row *WorkItem) error {
	err := query.Take(row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return store.ErrLeaseLost
	}
	return err
}

func workValue(row WorkItem) store.WorkItem {
	return store.WorkItem{
		ID: row.WorkID, Kind: store.WorkKind(row.Kind), ReadyAt: row.ReadyAt,
		OperationID: optionalID(row.OperationID), InstanceID: optionalID(row.InstanceID), ActivationID: optionalID(row.ActivationID),
	}
}

func optionalID(id *string) string {
	if id == nil {
		return ""
	}
	return *id
}
