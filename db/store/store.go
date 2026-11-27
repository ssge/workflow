// Package store defines the R1 runtime persistence port, independently of SQL
// and the migration models.
package store

import (
	"context"
	"errors"
)

var (
	ErrNotFound            = errors.New("store record not found")
	ErrIdempotencyConflict = errors.New("idempotency key already used for a different command")
	ErrConflict            = errors.New("store contention or stale state; retry from fresh data")
	ErrLeaseLost           = errors.New("work claim is no longer current")
	ErrInvalidArgument     = errors.New("invalid store argument")
)

// Clock returns authoritative UTC Unix milliseconds. Stores sharing a
// database must use the same time source.
type Clock func() int64

type Store interface {
	// WithTransaction commits on nil; otherwise it rolls back.
	WithTransaction(context.Context, func(Tx) error) error
	// Admit commits a pending receipt and command work, or replays the
	// original receipt and current outcome for an equal normalized command.
	Admit(context.Context, Admission) (Operation, error)
	// Claim takes up to Limit eligible items of one kind in a short
	// transaction. Rotate kinds outside the store to prevent starvation.
	Claim(context.Context, ClaimRequest) ([]Claim, error)
	// Apply checks the lease before running fn and again after its writes.
	// It marks the work done and finishes the associated command, if any, in
	// the same commit. fn must return an Outcome for command work and nil
	// for other work. Any error (including a lost lease) discards all writes.
	Apply(context.Context, Lease, func(Tx, WorkItem) (*Outcome, error)) error
	// RaiseIncident fences a separate failure commit for STEP or TIMER work.
	// It records the incident and its journal entry at the next revision of
	// the leased instance, and blocks the work without committing step effects.
	RaiseIncident(context.Context, Lease, IncidentRaise) error
}

// Tx is valid only inside its callback, for use by one goroutine. Use Apply
// for leased work; do not perform external effects in either callback.
// Propagate mutation errors; a missing head from CreateInstanceAtHead has
// made no writes and may be handled as a failed command.
type Tx interface {
	GetOperation(id string) (Operation, error)
	Publish(Publication) (DefinitionRef, error)
	GetDefinition(DefinitionRef) (Definition, error)
	// ListDefinitions returns up to limit versions strictly after afterVersion,
	// in ascending version order. Start at zero; an unknown key has no versions.
	ListDefinitions(processKey string, afterVersion int64, limit int) ([]DefinitionVersion, error)
	// GetHead reads the current ref without reserving it for a start.
	// An unknown key returns ErrNotFound.
	GetHead(processKey string) (DefinitionRef, error)
	// ListInstancePins returns all pins for the exact ref, ordered by instance ID.
	ListInstancePins(DefinitionRef) ([]InstancePin, error)
	// CountInstancePins counts pins for the exact ref in any of statuses;
	// no statuses counts all pins.
	CountInstancePins(ref DefinitionRef, statuses ...InstanceStatus) (int64, error)
	CreateInstanceAtHead(InstanceStart) (Instance, error)
	GetInstance(id string) (Instance, error)
	ChangeInstance(InstanceChange) error
	GetTransition(instanceID string, revision int64) (Transition, error)
	GetActivation(id string) (Activation, error)
	InsertActivation(Activation) error
	SetActivationState(id string, expected, next ActivationState) error
	EnqueueStep(StepWork) error

	InsertUserTask(NewUserTask) error
	GetUserTask(id string) (UserTask, error)
	GetUserTaskByActivation(activationID string) (UserTask, error)
	ListUserTasks(TaskQuery) ([]TaskRecord, error)
	// CloseUserTask changes an OPEN task to a terminal state.
	CloseUserTask(id string, closure TaskClosure) error

	InsertBoundaryTimer(NewBoundaryTimer) error
	GetBoundaryTimer(activationID string) (BoundaryTimer, error)
	// TransitionBoundaryTimer changes an ARMED timer to a terminal state.
	// It does not consume any work already enqueued for the timer.
	TransitionBoundaryTimer(activationID string, next TimerState) error
	// EnqueueTimerWork uses the armed timer's deadline as its ready time.
	EnqueueTimerWork(workID, activationID string) error

	CreateFork(NewFork) error
	GetFork(id string) (Fork, error)
	InsertBranchActivation(BranchActivation) error
	GetBranchActivation(activationID string) (BranchActivation, error)
	// InsertJoinArrival serializes arrival writes with closure of the fork.
	InsertJoinArrival(JoinArrival) error
	ListJoinArrivals(forkID string) ([]JoinArrival, error)
	// CloseFork changes an OPEN fork to CLOSED. The caller checks readiness.
	CloseFork(id string) error

	GetIncident(instanceID string, revision int64) (Incident, error)
}

// Object and Array carry complete JSON documents. Ingress/import owns
// normalization, duplicate-key checks and decimal-profile validation.
type Object string
type Array string

type CommandKind string

const (
	Deploy       CommandKind = "Deploy"
	Start        CommandKind = "Start"
	CompleteTask CommandKind = "CompleteTask"
)

type Command struct {
	Kind            CommandKind
	Target, Payload Object
}

type Admission struct {
	OperationID, WorkID, Key string
	Command                  Command
	ReadyAt                  int64
}

type OperationStatus string

const (
	OperationPending   OperationStatus = "PENDING"
	OperationSucceeded OperationStatus = "SUCCEEDED"
	OperationFailed    OperationStatus = "FAILED"
)

// Outcome for command completion has exactly one of Result (success) or
// Error (failure). Pending operations have neither.
type Outcome struct {
	Result, Error *Object
}

type Operation struct {
	ID      string
	Command Command
	Status  OperationStatus
	Outcome
}

type DefinitionRef struct {
	ProcessKey string
	Version    int64
}

// DefinitionVersion is catalog metadata without the sources or compiled plan.
type DefinitionVersion struct {
	Ref       DefinitionRef
	CreatedAt int64
}

type Publication struct {
	ProcessKey, ProfileRevision string
	Sources                     Array
	Plan                        Object
	CreatedAt                   int64
}

type Definition struct {
	Ref             DefinitionRef
	ProfileRevision string
	Sources         Array
	Plan            Object
	CreatedAt       int64
}

type InstanceStatus string

const (
	InstanceRunning   InstanceStatus = "RUNNING"
	InstanceIncident  InstanceStatus = "INCIDENT"
	InstanceCompleted InstanceStatus = "COMPLETED"
)

// InstancePin describes an instance pinned to the queried definition.
type InstancePin struct {
	InstanceID string
	Status     InstanceStatus
}

type Instance struct {
	ID        string
	Ref       DefinitionRef
	Status    InstanceStatus
	Revision  int64
	Variables Object
	CreatedAt int64
	EndedAt   *int64
}

// InstanceStart reads and serializes against the current head; creation
// writes revision 1 and a STARTED entry at CreatedAt.
type InstanceStart struct {
	ID, ProcessKey string
	Variables      Object
	CreatedAt      int64
}

// InstanceChange compares ExpectedRevision, then writes the next revision
// and its journal entry. A revision mismatch or terminal instance conflicts.
type InstanceChange struct {
	ID               string
	ExpectedRevision int64
	Status           InstanceStatus
	Variables        Object
	EndedAt          *int64
	Entry            JournalEntry
}

type TransitionKind string

const (
	TransitionStarted        TransitionKind = "STARTED"
	TransitionStep           TransitionKind = "STEP"
	TransitionIncidentRaised TransitionKind = "INCIDENT_RAISED"
	TransitionCompleted      TransitionKind = "COMPLETED"
)

type JournalEntry struct {
	Kind      TransitionKind
	ElementID *string
	AtMS      int64
}

type Transition struct {
	InstanceID string
	Revision   int64
	JournalEntry
}

type ActivationState string

const (
	ActivationReady    ActivationState = "READY"
	ActivationWaiting  ActivationState = "WAITING"
	ActivationConsumed ActivationState = "CONSUMED"
)

type Activation struct {
	ID, InstanceID, TokenID, ElementID string
	State                              ActivationState
}

type StepWork struct {
	ID, InstanceID, ActivationID string
	ReadyAt                      int64
}

type WorkKind string

const (
	WorkCommand WorkKind = "COMMAND"
	WorkStep    WorkKind = "STEP"
	WorkTimer   WorkKind = "TIMER"
)

// WorkItem identifies a work cause. References not belonging to the work
// kind are empty.
type WorkItem struct {
	ID                                    string
	Kind                                  WorkKind
	OperationID, InstanceID, ActivationID string
	ReadyAt                               int64
}

type ClaimRequest struct {
	Owner   string
	Kind    WorkKind
	Limit   int
	LeaseMS int64
}

// Lease identity is WorkID, Owner and Fence. ExpiresAt is informational;
// Apply uses the persisted expiry and the store clock.
type Lease struct {
	WorkID, Owner    string
	Fence, ExpiresAt int64
}

type Claim struct {
	Work  WorkItem
	Lease Lease
}

type NewUserTask struct {
	ID, ActivationID, FormID string
	Components               Array
	CreatedAt                int64
}

type TaskStatus string

const (
	TaskOpen      TaskStatus = "OPEN"
	TaskCompleted TaskStatus = "COMPLETED"
	TaskCancelled TaskStatus = "CANCELLED"
)

type TaskCloseReason string

const TaskCancelledByTimer TaskCloseReason = "TIMER"

type TaskClosure struct {
	Status   TaskStatus
	ClosedAt int64
	Reason   *TaskCloseReason
}

type UserTask struct {
	NewUserTask
	Status   TaskStatus
	ClosedAt *int64
	Reason   *TaskCloseReason
}

type TaskQuery struct {
	InstanceID, Status string
	AfterCreatedAt     *int64
	AfterID            string
	Limit              int
}

type TaskRecord struct {
	Task      UserTask
	Instance  Instance
	ElementID string
	DueAt     *int64
}

type NewBoundaryTimer struct {
	ID, ActivationID, ElementID string
	DueAt                       int64
}

type TimerState string

const (
	TimerArmed     TimerState = "ARMED"
	TimerFired     TimerState = "FIRED"
	TimerCancelled TimerState = "CANCELLED"
)

type BoundaryTimer struct {
	NewBoundaryTimer
	State TimerState
}

type NewFork struct {
	ID, InstanceID, SplitActivationID, JoinElementID string
}

type ForkState string

const (
	ForkOpen   ForkState = "OPEN"
	ForkClosed ForkState = "CLOSED"
)

type Fork struct {
	NewFork
	State ForkState
}

type BranchActivation struct {
	ActivationID, InstanceID, ForkID, BranchFlowID string
}

type JoinArrival struct {
	ForkID, IncomingFlowID, BranchFlowID, ActivationID string
}

type IncidentRaise struct {
	ExpectedRevision, AtMS   int64
	ElementID, Code, Message string
	RetryInput               Object
}

type Incident struct {
	InstanceID, WorkID       string
	Revision                 int64
	ElementID, Code, Message string
	RetryInput               Object
}
