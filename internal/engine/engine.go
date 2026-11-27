// Package engine coordinates durable R1 commands and leased token transitions.
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ssge/workflow/db/store"
	"github.com/ssge/workflow/internal/application"
	"github.com/ssge/workflow/internal/clock"
	"github.com/ssge/workflow/internal/evaluator"
)

// Engine implements the application command and query service.
type Engine struct {
	store store.Store
	clock clock.Clock
}

var _ application.Service = (*Engine)(nil)

// New binds the application service to an initialized store. The clock must
// use the same time source as the store and all other engines on that store.
func New(s store.Store, c clock.Clock) *Engine {
	return &Engine{store: s, clock: c}
}

func (e *Engine) Accept(ctx context.Context, key string, command application.Command) (application.Receipt, error) {
	if !validKey(key) {
		return application.Receipt{}, serviceError(400, "INVALID_REQUEST", "invalid idempotency key")
	}
	normalized, err := normalize(command)
	if err != nil {
		return application.Receipt{}, serviceError(400, "INVALID_REQUEST", err.Error())
	}
	id := operationID(key)
	op, err := e.store.Admit(ctx, store.Admission{
		OperationID: id, WorkID: "command-" + id, Key: key, Command: normalized, ReadyAt: e.clock.Now().UnixMilli(),
	})
	if errors.Is(err, store.ErrIdempotencyConflict) {
		return application.Receipt{}, serviceError(409, "IDEMPOTENCY_CONFLICT", "idempotency key already used for a different command")
	}
	if err != nil {
		return application.Receipt{}, unavailable()
	}
	return application.Receipt{OperationID: op.ID, Status: string(op.Status)}, nil
}

// IDs derived from the key let the engine read its admissions by key using
// the store's ID lookup, without a second index outside the inbox transaction.
func operationID(key string) string {
	return fmt.Sprintf("op-%x", sha256.Sum256([]byte(key)))
}

func validKey(key string) bool {
	if len(key) == 0 || len(key) > 128 {
		return false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < '!' || key[i] > '~' {
			return false
		}
	}
	return true
}

func normalize(command application.Command) (store.Command, error) {
	payload, err := evaluator.ParseVariables(command.Payload)
	if err != nil {
		return store.Command{}, fmt.Errorf("invalid command payload: %w", err)
	}
	target := make(map[string]any, len(command.Target))
	for k, v := range command.Target {
		target[k] = v
	}
	switch command.Kind {
	case application.Deploy:
		files, ok := payload["files"].([]any)
		if len(target) != 0 || len(payload) != 1 || !ok {
			return store.Command{}, errors.New("Deploy requires a files array and no target")
		}
		for _, raw := range files {
			file, _ := raw.(map[string]any)
			_, nameOK := file["name"].(string)
			_, contentOK := file["content"].(string)
			if len(file) != 2 || !nameOK || !contentOK {
				return store.Command{}, errors.New("each file requires a string name and content")
			}
		}
	case application.Start:
		key, _ := payload["processKey"].(string)
		if _, present := payload["variables"]; !present {
			payload["variables"] = map[string]any{}
		}
		_, object := payload["variables"].(map[string]any)
		if len(target) != 0 || len(payload) != 2 || key == "" || !object {
			return store.Command{}, errors.New("Start requires a processKey, optional variables object, and no target")
		}
	case application.CompleteTask:
		fields, ok := payload["fields"].(map[string]any)
		if len(target) != 1 || command.Target["taskId"] == "" || len(payload) != 1 || !ok {
			return store.Command{}, errors.New("CompleteTask requires a taskId target and a fields object")
		}
		for _, value := range fields {
			if _, ok := value.(string); !ok {
				return store.Command{}, errors.New("task field values must be strings")
			}
		}
	default:
		return store.Command{}, errors.New("unknown command kind")
	}
	p, err := evaluator.MarshalVariables(payload)
	if err != nil {
		return store.Command{}, err
	}
	t, err := evaluator.MarshalVariables(target)
	if err != nil {
		return store.Command{}, err
	}
	return store.Command{Kind: store.CommandKind(command.Kind), Target: store.Object(t), Payload: store.Object(p)}, nil
}

func serviceError(status int, code, message string) *application.Error {
	return &application.Error{Status: status, Failure: application.Failure{Code: code, Message: message}}
}

func unavailable() *application.Error {
	return serviceError(503, "UNAVAILABLE", "operation could not be confirmed")
}

// encodeObject is for typed internal results and plans. Arbitrary ingress
// JSON must go through the evaluator instead.
func encodeObject(value any) (store.Object, error) {
	raw, err := json.Marshal(value)
	return store.Object(raw), err
}
