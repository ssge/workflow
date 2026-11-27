package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ssge/workflow/internal/application"
)

func TestBrokerReceiveAcksDurableAdmission(t *testing.T) {
	service := &fakeService{}
	adapter := &Handler{service: service}

	got := adapter.Receive(context.Background(), []byte(`{
		"schemaVersion":1,"key":"start-1","kind":"Start","target":{},
		"payload":{"processKey":"p"}
	}`))
	if got.Action != Ack || got.OperationID != "op-7" {
		t.Fatalf("delivery result = %+v", got)
	}
	if service.command.Kind != application.Start || service.key != "start-1" {
		t.Fatalf("accepted command = %+v, key %q", service.command, service.key)
	}
	var payload struct {
		ProcessKey string          `json:"processKey"`
		Variables  json.RawMessage `json:"variables"`
	}
	if err := json.Unmarshal(service.command.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ProcessKey != "p" || string(payload.Variables) != "{}" {
		t.Fatalf("normalized payload = %s", service.command.Payload)
	}
}

func TestBrokerReceiveRejectsInvalidEnvelopes(t *testing.T) {
	for _, body := range []string{
		`{"schemaVersion":2,"key":"k","kind":"Start","target":{},"payload":{"processKey":"p"}}`,
		`{"schemaVersion":1,"key":"has space","kind":"Start","target":{},"payload":{"processKey":"p"}}`,
		`{"schemaVersion":1,"key":"k","kind":"Unknown","target":{},"payload":{}}`,
		`{"schemaVersion":1,"key":"k","kind":"Start","target":{"instanceId":"i"},"payload":{"processKey":"p"}}`,
		`{"schemaVersion":1,"key":"k","kind":"Start","target":{},"payload":{"processKey":"p","processKey":"q"}}`,
		`{"schemaVersion":1,"key":"k","kind":"Start","target":{},"payload":{"processKey":"p","variables":{"x":1.12345678901234567890123456789012345}}}`,
		`{"schemaVersion":1,"key":"k","kind":"CompleteTask","target":{"taskId":"t"},"payload":{"fields":{"x":null}}}`,
	} {
		service := &fakeService{}
		result := (&Handler{service: service}).Receive(context.Background(), []byte(body))
		if result.Action != Reject || result.Code != "INVALID_ENVELOPE" || service.key != "" {
			t.Errorf("%s: result=%+v accepted=%+v", body, result, service.command)
		}
	}
}

func TestBrokerReceiveDistinguishesConflictFromStoreFailure(t *testing.T) {
	service := &fakeService{acceptErr: &application.Error{
		Status:  http.StatusConflict,
		Failure: application.Failure{Code: "IDEMPOTENCY_CONFLICT", Message: "key conflict"},
	}}
	handler := &Handler{service: service}
	body := []byte(`{"schemaVersion":1,"key":"k","kind":"Start","target":{},"payload":{"processKey":"p"}}`)
	result := handler.Receive(context.Background(), body)
	if result.Action != Reject || result.Code != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("conflict result = %+v", result)
	}
	service.acceptErr = &application.Error{
		Status:  http.StatusServiceUnavailable,
		Failure: application.Failure{Code: "UNAVAILABLE", Message: "store offline"},
	}
	result = handler.Receive(context.Background(), body)
	if result.Action != Retry || result.Code != "" {
		t.Fatalf("store failure result = %+v", result)
	}
}

func TestBrokerRejectsCaseMismatch(t *testing.T) {
	service := &fakeService{}
	body := strings.Replace(`{"schemaVersion":1,"key":"k","kind":"Start","target":{},"payload":{"processKey":"p"}}`,
		`"processKey"`, `"ProcessKey"`, 1)
	result := (&Handler{service: service}).Receive(context.Background(), []byte(body))
	if result.Action != Reject || service.key != "" {
		t.Fatalf("result=%+v accepted=%+v", result, service.command)
	}
}
