package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"

	"github.com/ssge/workflow/internal/application"
	"github.com/ssge/workflow/internal/evaluator"
)

type DeliveryAction string

const (
	Ack    DeliveryAction = "ack"
	Retry  DeliveryAction = "retry"
	Reject DeliveryAction = "reject"
)

type DeliveryResult struct {
	Action      DeliveryAction `json:"action"`
	OperationID string         `json:"operationId,omitempty"`
	Code        string         `json:"code,omitempty"`
}

type deliveryEnvelope struct {
	SchemaVersion *int                        `json:"schemaVersion"`
	Key           *string                     `json:"key"`
	Kind          *string                     `json:"kind"`
	Target        *map[string]json.RawMessage `json:"target"`
	Payload       json.RawMessage             `json:"payload"`
}

func (h *Handler) Receive(ctx context.Context, body []byte) DeliveryResult {
	var envelope deliveryEnvelope
	if err := decodeStrictJSON(body, &envelope); err != nil ||
		envelope.SchemaVersion == nil || *envelope.SchemaVersion != 1 ||
		envelope.Key == nil || !validCommandKey(*envelope.Key) ||
		envelope.Kind == nil || envelope.Target == nil || len(envelope.Payload) == 0 {
		return DeliveryResult{Action: Reject, Code: "INVALID_ENVELOPE"}
	}

	command, ok := decodeDeliveryCommand(*envelope.Kind, *envelope.Target, envelope.Payload)
	if !ok {
		return DeliveryResult{Action: Reject, Code: "INVALID_ENVELOPE"}
	}
	receipt, err := h.service.Accept(ctx, *envelope.Key, command)
	if err == nil {
		if receipt.OperationID == "" || (receipt.Status != "PENDING" && receipt.Status != "SUCCEEDED" && receipt.Status != "FAILED") {
			log.Printf("workflow broker received an invalid admission receipt: %+v", receipt)
			return DeliveryResult{Action: Retry}
		}
		return DeliveryResult{Action: Ack, OperationID: receipt.OperationID}
	}
	var serviceError *application.Error
	if errors.As(err, &serviceError) {
		if serviceError.Status >= 500 {
			return DeliveryResult{Action: Retry}
		}
		if serviceError.Status >= 400 {
			return DeliveryResult{Action: Reject, Code: serviceError.Failure.Code}
		}
	}
	log.Printf("workflow broker admission error: %v", err)
	return DeliveryResult{Action: Retry}
}

func decodeDeliveryCommand(kind string, target map[string]json.RawMessage, payload json.RawMessage) (application.Command, bool) {
	command := application.Command{}
	switch kind {
	case string(application.Deploy):
		var request deployRequest
		if err := decodeStrictJSON(payload, &request); err != nil || request.Files == nil || !emptyObject(target) {
			return command, false
		}
		for _, file := range *request.Files {
			if file.Name == nil || file.Content == nil {
				return command, false
			}
		}
		command = application.Command{Kind: application.Deploy, Target: map[string]string{}, Payload: payload}
	case string(application.Start):
		var request startRequest
		if err := decodeStrictJSON(payload, &request); err != nil || request.ProcessKey == nil || *request.ProcessKey == "" || !emptyObject(target) {
			return command, false
		}
		if len(request.Variables) == 0 {
			request.Variables = json.RawMessage(`{}`)
		}
		if _, err := evaluator.ParseVariables(request.Variables); err != nil {
			return command, false
		}
		normalized, err := json.Marshal(struct {
			ProcessKey string          `json:"processKey"`
			Variables  json.RawMessage `json:"variables"`
		}{ProcessKey: *request.ProcessKey, Variables: request.Variables})
		if err != nil {
			return command, false
		}
		command = application.Command{Kind: application.Start, Target: map[string]string{}, Payload: normalized}
	case string(application.CompleteTask):
		taskID, valid := exactStringTarget(target, "taskId")
		if !valid {
			return command, false
		}
		var request completeRequest
		if err := decodeStrictJSON(payload, &request); err != nil || request.Fields == nil {
			return command, false
		}
		fields := make(map[string]string, len(*request.Fields))
		for key, raw := range *request.Fields {
			value, valid := jsonString(raw)
			if !valid {
				return command, false
			}
			fields[key] = value
		}
		normalized, err := json.Marshal(struct {
			Fields map[string]string `json:"fields"`
		}{Fields: fields})
		if err != nil {
			return command, false
		}
		command = application.Command{Kind: application.CompleteTask, Target: map[string]string{"taskId": taskID}, Payload: normalized}
	default:
		return command, false
	}
	return command, true
}

func emptyObject(value map[string]json.RawMessage) bool {
	return value != nil && len(value) == 0
}

func exactStringTarget(target map[string]json.RawMessage, name string) (string, bool) {
	if len(target) != 1 {
		return "", false
	}
	raw, exists := target[name]
	if !exists {
		return "", false
	}
	value, valid := jsonString(raw)
	return value, valid && value != ""
}

func jsonString(raw json.RawMessage) (string, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}
