package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/ssge/workflow/internal/application"
)

type fakeService struct {
	command      application.Command
	key          string
	acceptErr    error
	taskStatus   string
	taskInstance string
}

func (s *fakeService) Accept(_ context.Context, key string, command application.Command) (application.Receipt, error) {
	s.key, s.command = key, command
	return application.Receipt{OperationID: "op-7", Status: "PENDING"}, s.acceptErr
}

func (*fakeService) GetOperation(context.Context, string) (application.Operation, error) {
	return application.Operation{}, nil
}

func (*fakeService) GetOperationByKey(context.Context, string) (application.Operation, error) {
	return application.Operation{}, nil
}

func (*fakeService) ListDefinitionVersions(context.Context, string, int, string) (application.Page[application.DefinitionVersion], error) {
	return application.Page[application.DefinitionVersion]{}, nil
}

func (*fakeService) GetInstance(context.Context, string) (application.Instance, error) {
	return application.Instance{}, nil
}

func (s *fakeService) ListTasks(_ context.Context, instanceID, status string, _ int, _ string) (application.Page[application.TaskSummary], error) {
	s.taskInstance, s.taskStatus = instanceID, status
	return application.Page[application.TaskSummary]{}, nil
}

func (*fakeService) GetTask(context.Context, string) (application.Task, error) {
	return application.Task{}, nil
}

func TestDeployAdmission(t *testing.T) {
	service := &fakeService{}
	handler := NewHandler(service)
	request := httptest.NewRequest(http.MethodPost, "/v1/deployments", strings.NewReader(`{"files":[{"name":"p.bpmn","content":"<definitions/>"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "deploy-1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if got := response.Header().Get("Location"); got != "/v1/operations/op-7" {
		t.Fatalf("Location = %q", got)
	}
	if service.key != "deploy-1" || service.command.Kind != application.Deploy {
		t.Fatalf("accepted command = %+v, key %q", service.command, service.key)
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(body, map[string]any{"operationId": "op-7", "status": "PENDING"}) {
		t.Fatalf("receipt = %#v", body)
	}
}

func TestCommandRejectsMalformedRequestsBeforeAdmission(t *testing.T) {
	tests := []struct {
		name        string
		key         string
		body        string
		contentType string
	}{
		{"missing key", "", `{"processKey":"p"}`, "application/json"},
		{"invalid key", "not printable", `{"processKey":"p"}`, "application/json"},
		{"duplicate nested variable", "start-1", `{"processKey":"p","variables":{"x":1,"x":2}}`, "application/json"},
		{"non-object variables", "start-2", `{"processKey":"p","variables":null}`, "application/json"},
		{"unknown property", "start-3", `{"processKey":"p","extra":true}`, "application/json"},
		{"wrong content type", "start-4", `{"processKey":"p"}`, "text/plain"},
		{"out of profile number", "start-5", `{"processKey":"p","variables":{"x":1.12345678901234567890123456789012345}}`, "application/json"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &fakeService{}
			handler := NewHandler(service)
			request := httptest.NewRequest(http.MethodPost, "/v1/instances", strings.NewReader(test.body))
			request.Header.Set("Content-Type", test.contentType)
			if test.key != "" {
				request.Header.Set("Idempotency-Key", test.key)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body)
			}
			if service.key != "" {
				t.Fatalf("malformed command reached service: %+v", service.command)
			}
		})
	}
}

func TestStartDefaultsVariablesToEmptyObject(t *testing.T) {
	service := &fakeService{}
	handler := NewHandler(service)
	request := httptest.NewRequest(http.MethodPost, "/v1/instances", strings.NewReader(`{"processKey":"p"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "start-1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	var payload struct {
		ProcessKey string          `json:"processKey"`
		Variables  json.RawMessage `json:"variables"`
	}
	if err := json.Unmarshal(service.command.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ProcessKey != "p" || string(payload.Variables) != "{}" {
		t.Fatalf("payload = %s", service.command.Payload)
	}
}

func TestDeployRequiresFileNameAndContent(t *testing.T) {
	service := &fakeService{}
	handler := NewHandler(service)
	request := httptest.NewRequest(http.MethodPost, "/v1/deployments", strings.NewReader(`{"files":[{"content":"<definitions/>"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "deploy-1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || service.key != "" {
		t.Fatalf("status=%d accepted=%+v", response.Code, service.command)
	}
}

func TestCompleteTaskRejectsNullFieldAndPassesTarget(t *testing.T) {
	for _, body := range []string{`{"fields":{"command":null}}`, `{"fields":null}`} {
		service := &fakeService{}
		handler := NewHandler(service)
		request := httptest.NewRequest(http.MethodPost, "/v1/tasks/t-1/complete", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", "complete-1")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest || service.key != "" {
			t.Fatalf("body %s: status=%d accepted=%+v", body, response.Code, service.command)
		}
	}

	service := &fakeService{}
	handler := NewHandler(service)
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks/t-1/complete", strings.NewReader(`{"fields":{}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "complete-2")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if service.command.Target["taskId"] != "t-1" {
		t.Fatalf("target = %#v", service.command.Target)
	}
}

func TestTaskListDefaultsAndRejectsBadFilters(t *testing.T) {
	service := &fakeService{}
	handler := NewHandler(service)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/tasks?instanceId=i-1", nil))
	if response.Code != http.StatusOK || service.taskStatus != "OPEN" || service.taskInstance != "i-1" {
		t.Fatalf("status=%d filters=(%q,%q)", response.Code, service.taskInstance, service.taskStatus)
	}
	if got := response.Body.String(); !strings.Contains(got, `"items":[]`) || !strings.Contains(got, `"nextCursor":null`) {
		t.Fatalf("empty page shape = %s", got)
	}

	for _, path := range []string{
		"/v1/tasks?status=INVALID",
		"/v1/tasks?status=",
		"/v1/tasks?limit=",
		"/v1/tasks?limit=0",
		"/v1/tasks?limit=10&limit=20",
		"/v1/tasks?extra=x",
		"/v1/tasks?status=%ZZ",
		"/v1/tasks?cursor=%ZZ",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d", path, response.Code)
		}
	}
}

func TestServiceErrorsAreExplicit(t *testing.T) {
	service := &fakeService{acceptErr: &application.Error{
		Status:  http.StatusConflict,
		Failure: application.Failure{Code: "IDEMPOTENCY_CONFLICT", Message: "Key conflict."},
	}}
	handler := NewHandler(service)
	newRequest := func() *http.Request {
		request := httptest.NewRequest(http.MethodPost, "/v1/deployments", strings.NewReader(`{"files":[]}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", "deploy-1")
		return request
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, newRequest())
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}

	service.acceptErr = errors.New("database failed")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, newRequest())
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
}
