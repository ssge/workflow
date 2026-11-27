package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/ssge/workflow/internal/application"
	"github.com/ssge/workflow/internal/evaluator"
)

type Handler struct {
	service application.Service
}

func NewHandler(service application.Service) http.Handler {
	mux := http.NewServeMux()
	handler := &Handler{service: service}

	mux.HandleFunc("/v1/deployments", handler.deploy)
	mux.HandleFunc("/v1/instances", handler.instances)
	mux.HandleFunc("/v1/tasks", handler.tasks)
	mux.HandleFunc("/v1/operations/{id}", handler.operation)
	mux.HandleFunc("/v1/operations/by-key/{urlEncodedKey}", handler.operationByKey)
	mux.HandleFunc("/v1/definitions/{processKey}/versions", handler.definitionVersions)
	mux.HandleFunc("/v1/instances/{instanceId}", handler.instance)
	mux.HandleFunc("/v1/tasks/{taskId}", handler.task)
	mux.HandleFunc("/v1/tasks/{taskId}/complete", handler.completeTask)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeFailure(w, http.StatusNotFound, application.Failure{Code: "NOT_FOUND", Message: "Unknown route."})
	})
	return mux
}

type sourceRequest struct {
	Name    *string `json:"name"`
	Content *string `json:"content"`
}

type deployRequest struct {
	Files *[]sourceRequest `json:"files"`
}

type startRequest struct {
	ProcessKey *string         `json:"processKey"`
	Variables  json.RawMessage `json:"variables"`
}

type completeRequest struct {
	Fields *map[string]json.RawMessage `json:"fields"`
}

func (h *Handler) deploy(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	key, ok := commandKey(w, r)
	if !ok {
		return
	}
	body, ok := requestBody(w, r)
	if !ok {
		return
	}
	var request deployRequest
	if err := decodeStrictJSON(body, &request); err != nil || request.Files == nil {
		writeInvalidRequest(w)
		return
	}
	for _, file := range *request.Files {
		if file.Name == nil || file.Content == nil {
			writeInvalidRequest(w)
			return
		}
	}
	receipt, err := h.service.Accept(r.Context(), key, application.Command{
		Kind:    application.Deploy,
		Target:  map[string]string{},
		Payload: body,
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeReceipt(w, receipt)
}

func (h *Handler) instances(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	key, ok := commandKey(w, r)
	if !ok {
		return
	}
	body, ok := requestBody(w, r)
	if !ok {
		return
	}
	var request startRequest
	if err := decodeStrictJSON(body, &request); err != nil || request.ProcessKey == nil || *request.ProcessKey == "" {
		writeInvalidRequest(w)
		return
	}
	if len(request.Variables) == 0 {
		request.Variables = json.RawMessage(`{}`)
	}
	if _, err := evaluator.ParseVariables(request.Variables); err != nil {
		writeInvalidRequest(w)
		return
	}
	payload, err := json.Marshal(struct {
		ProcessKey string          `json:"processKey"`
		Variables  json.RawMessage `json:"variables"`
	}{ProcessKey: *request.ProcessKey, Variables: request.Variables})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	receipt, err := h.service.Accept(r.Context(), key, application.Command{
		Kind:    application.Start,
		Target:  map[string]string{},
		Payload: payload,
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeReceipt(w, receipt)
}

func (h *Handler) completeTask(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	taskID := r.PathValue("taskId")
	if taskID == "" {
		writeInvalidRequest(w)
		return
	}
	key, ok := commandKey(w, r)
	if !ok {
		return
	}
	body, ok := requestBody(w, r)
	if !ok {
		return
	}
	var request completeRequest
	if err := decodeStrictJSON(body, &request); err != nil || request.Fields == nil {
		writeInvalidRequest(w)
		return
	}
	fields := make(map[string]string, len(*request.Fields))
	for name, raw := range *request.Fields {
		trimmed := strings.TrimSpace(string(raw))
		if len(trimmed) == 0 || trimmed[0] != '"' {
			writeInvalidRequest(w)
			return
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			writeInvalidRequest(w)
			return
		}
		fields[name] = value
	}
	payload, err := json.Marshal(struct {
		Fields map[string]string `json:"fields"`
	}{Fields: fields})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	receipt, err := h.service.Accept(r.Context(), key, application.Command{
		Kind:    application.CompleteTask,
		Target:  map[string]string{"taskId": taskID},
		Payload: payload,
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeReceipt(w, receipt)
}

func (h *Handler) operation(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeInvalidRequest(w)
		return
	}
	result, err := h.service.GetOperation(r.Context(), id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) operationByKey(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	key := r.PathValue("urlEncodedKey")
	if !validCommandKey(key) {
		writeInvalidRequest(w)
		return
	}
	result, err := h.service.GetOperationByKey(r.Context(), key)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) definitionVersions(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	processKey := r.PathValue("processKey")
	if processKey == "" {
		writeInvalidRequest(w)
		return
	}
	limit, cursor, ok := pageParameters(w, r, map[string]bool{})
	if !ok {
		return
	}
	result, err := h.service.ListDefinitionVersions(r.Context(), processKey, limit, cursor)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNilItems(result))
}

func (h *Handler) instance(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	id := r.PathValue("instanceId")
	if id == "" {
		writeInvalidRequest(w)
		return
	}
	result, err := h.service.GetInstance(r.Context(), id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) tasks(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeInvalidRequest(w)
		return
	}
	if !validateQueryKeys(w, query, "instanceId", "status", "limit", "cursor") {
		return
	}
	instanceID, ok := oneQueryValue(w, query, "instanceId")
	if !ok {
		return
	}
	if values, exists := query["instanceId"]; exists && (len(values) != 1 || values[0] == "") {
		writeInvalidRequest(w)
		return
	}
	status, ok := oneQueryValue(w, query, "status")
	if !ok {
		return
	}
	if values, exists := query["status"]; exists && (len(values) != 1 || values[0] == "") {
		writeInvalidRequest(w)
		return
	}
	limit, cursor, ok := pageParameters(w, r, map[string]bool{"instanceId": true, "status": true})
	if !ok {
		return
	}
	if _, exists := query["status"]; !exists {
		status = "OPEN"
	}
	if status != "OPEN" && status != "COMPLETED" && status != "CANCELLED" {
		writeInvalidRequest(w)
		return
	}
	result, err := h.service.ListTasks(r.Context(), instanceID, status, limit, cursor)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNilItems(result))
}

func (h *Handler) task(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	id := r.PathValue("taskId")
	if id == "" {
		writeInvalidRequest(w)
		return
	}
	result, err := h.service.GetTask(r.Context(), id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func pageParameters(w http.ResponseWriter, r *http.Request, allowed map[string]bool) (int, string, bool) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeInvalidRequest(w)
		return 0, "", false
	}
	allowed["limit"] = true
	allowed["cursor"] = true
	if !validateQueryKeys(w, query, mapKeys(allowed)...) {
		return 0, "", false
	}
	limitText, ok := oneQueryValue(w, query, "limit")
	if !ok {
		return 0, "", false
	}
	limit := 50
	if limitText != "" {
		parsed, err := strconv.Atoi(limitText)
		if err != nil || parsed < 1 || parsed > 200 {
			writeInvalidRequest(w)
			return 0, "", false
		}
		limit = parsed
	} else if _, exists := query["limit"]; exists {
		writeInvalidRequest(w)
		return 0, "", false
	}
	cursor, ok := oneQueryValue(w, query, "cursor")
	if !ok {
		return 0, "", false
	}
	return limit, cursor, true
}

func mapKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func validateQueryKeys(w http.ResponseWriter, values url.Values, allowed ...string) bool {
	accepted := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		accepted[key] = true
	}
	for key := range values {
		if !accepted[key] {
			writeInvalidRequest(w)
			return false
		}
	}
	return true
}

func oneQueryValue(w http.ResponseWriter, values url.Values, key string) (string, bool) {
	all, exists := values[key]
	if !exists {
		return "", true
	}
	if len(all) != 1 {
		writeInvalidRequest(w)
		return "", false
	}
	return all[0], true
}

func requestBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	contentType := r.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "application/json" {
		writeInvalidRequest(w)
		return nil, false
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeInvalidRequest(w)
		return nil, false
	}
	return body, true
}

func commandKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.Header.Get("Idempotency-Key")
	if !validCommandKey(key) {
		writeInvalidRequest(w)
		return "", false
	}
	return key, true
}

func validCommandKey(key string) bool {
	if len(key) < 1 || len(key) > 128 {
		return false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < '!' || key[i] > '~' {
			return false
		}
	}
	return true
}

func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	writeFailure(w, http.StatusMethodNotAllowed, application.Failure{Code: "INVALID_REQUEST", Message: "Method not allowed."})
	return false
}

func writeReceipt(w http.ResponseWriter, receipt application.Receipt) {
	if receipt.OperationID == "" || (receipt.Status != "PENDING" && receipt.Status != "SUCCEEDED" && receipt.Status != "FAILED") {
		log.Printf("workflow API returned an invalid admission receipt: %+v", receipt)
		writeFailure(w, http.StatusServiceUnavailable, application.Failure{
			Code:    "UNAVAILABLE",
			Message: "The request could not be confirmed.",
		})
		return
	}
	w.Header().Set("Location", "/v1/operations/"+url.PathEscape(receipt.OperationID))
	writeJSON(w, http.StatusAccepted, receipt)
}

func writeServiceError(w http.ResponseWriter, err error) {
	var serviceError *application.Error
	if errors.As(err, &serviceError) && serviceError.Status >= 400 && serviceError.Status <= 599 {
		writeFailure(w, serviceError.Status, serviceError.Failure)
		return
	}
	log.Printf("workflow API service error: %v", err)
	writeFailure(w, http.StatusServiceUnavailable, application.Failure{
		Code:    "UNAVAILABLE",
		Message: "The request could not be confirmed.",
	})
}

func writeInvalidRequest(w http.ResponseWriter) {
	writeFailure(w, http.StatusBadRequest, application.Failure{Code: "INVALID_REQUEST", Message: "Invalid request."})
}

func writeInternalError(w http.ResponseWriter, err error) {
	log.Printf("workflow API response encoding error: %v", err)
	writeFailure(w, http.StatusInternalServerError, application.Failure{Code: "INTERNAL_ERROR", Message: "Internal server error."})
}

func writeFailure(w http.ResponseWriter, status int, failure application.Failure) {
	writeJSON(w, status, struct {
		Error application.Failure `json:"error"`
	}{Error: failure})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		log.Printf("workflow API response encoding error: %v", err)
		status = http.StatusInternalServerError
		body = []byte(`{"error":{"code":"INTERNAL_ERROR","message":"Internal server error."}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(append(body, '\n')); err != nil {
		log.Printf("workflow API response write error: %v", err)
	}
}

func nonNilItems[T any](page application.Page[T]) application.Page[T] {
	if page.Items == nil {
		page.Items = []T{}
	}
	return page
}
