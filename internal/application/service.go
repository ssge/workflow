package application

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

type CommandKind string

const (
	Deploy       CommandKind = "Deploy"
	Start        CommandKind = "Start"
	CompleteTask CommandKind = "CompleteTask"
)

type Command struct {
	Kind    CommandKind
	Target  map[string]string
	Payload json.RawMessage
}

type Receipt struct {
	OperationID string `json:"operationId"`
	Status      string `json:"status"`
}

type Operation struct {
	OperationID string          `json:"operationId"`
	Status      string          `json:"status"`
	Result      json.RawMessage `json:"result,omitempty"`
	Error       *Failure        `json:"error,omitempty"`
}

type Failure struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

type DefinitionVersion struct {
	ProcessKey string    `json:"processKey"`
	Version    int64     `json:"version"`
	Label      string    `json:"label"`
	CreatedAt  time.Time `json:"createdAt"`
}

type Page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"nextCursor"`
}

type Incident struct {
	Code      string `json:"code"`
	ElementID string `json:"elementId"`
	Message   string `json:"message"`
}

type Instance struct {
	InstanceID string          `json:"instanceId"`
	ProcessKey string          `json:"processKey"`
	Version    int64           `json:"version"`
	Status     string          `json:"status"`
	Revision   int64           `json:"revision"`
	Variables  json.RawMessage `json:"variables"`
	CreatedAt  time.Time       `json:"createdAt"`
	EndedAt    *time.Time      `json:"endedAt"`
	Incident   *Incident       `json:"incident"`
}

type TaskSummary struct {
	TaskID     string     `json:"taskId"`
	InstanceID string     `json:"instanceId"`
	ElementID  string     `json:"elementId"`
	Version    int64      `json:"version"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"createdAt"`
	DueAt      *time.Time `json:"dueAt"`
}

type Task struct {
	TaskSummary
	FormID     string          `json:"formId"`
	Components json.RawMessage `json:"components"`
	ClosedAt   *time.Time      `json:"closedAt"`
	Reason     *string         `json:"reason"`
}

type Service interface {
	Accept(context.Context, string, Command) (Receipt, error)
	GetOperation(context.Context, string) (Operation, error)
	GetOperationByKey(context.Context, string) (Operation, error)
	ListDefinitionVersions(context.Context, string, int, string) (Page[DefinitionVersion], error)
	GetInstance(context.Context, string) (Instance, error)
	ListTasks(context.Context, string, string, int, string) (Page[TaskSummary], error)
	GetTask(context.Context, string) (Task, error)
}

type Error struct {
	Status  int
	Failure Failure
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Failure.Code, e.Failure.Message)
}
