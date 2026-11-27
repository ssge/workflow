package release1

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type CamundaConfig struct {
	Endpoint    string
	BearerToken string
	Version     string
	Client      *http.Client
	Poll        PollConfig
	TimerWait   time.Duration
}

func CamundaConfigFromEnv() (CamundaConfig, bool, error) {
	endpoint := strings.TrimSpace(os.Getenv("CAMUNDA8_REST_URL"))
	if endpoint == "" {
		return CamundaConfig{}, false, nil
	}
	interval, err := durationEnv("CAMUNDA8_POLL_INTERVAL", 250*time.Millisecond)
	if err != nil {
		return CamundaConfig{}, false, err
	}
	timeout, err := durationEnv("CAMUNDA8_QUERY_TIMEOUT", 75*time.Second)
	if err != nil {
		return CamundaConfig{}, false, err
	}
	timerWait, err := durationEnv("CAMUNDA8_TIMER_WAIT", 75*time.Second)
	if err != nil {
		return CamundaConfig{}, false, err
	}
	config := CamundaConfig{
		Endpoint: endpoint, BearerToken: os.Getenv("CAMUNDA8_BEARER_TOKEN"),
		Version: os.Getenv("CAMUNDA8_VERSION"),
		Poll: PollConfig{
			Interval: interval,
			Timeout:  timeout,
		},
		TimerWait: timerWait,
	}
	if _, err := NewCamundaDriver(config); err != nil {
		return CamundaConfig{}, false, err
	}
	if config.Version == "" {
		config.Version = "unreported"
	}
	return config, true, nil
}

func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid %s duration %q: %w", name, value, err)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return duration, nil
}

type CamundaDriver struct {
	config      CamundaConfig
	client      *http.Client
	poll        PollConfig
	mu          sync.RWMutex
	definitions map[string]Definition
}

func NewCamundaDriver(config CamundaConfig) (*CamundaDriver, error) {
	parsed, err := url.Parse(strings.TrimRight(config.Endpoint, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid Camunda REST URL %q", config.Endpoint)
	}
	if config.Client == nil {
		config.Client = http.DefaultClient
	}
	if config.Version == "" {
		config.Version = "unreported"
	}
	if config.TimerWait <= 0 {
		config.TimerWait = config.Poll.normalized().Timeout
	}
	return &CamundaDriver{
		config: config, client: config.Client, poll: config.Poll.normalized(),
		definitions: make(map[string]Definition),
	}, nil
}

func (d *CamundaDriver) Backend() string { return "camunda8" }
func (d *CamundaDriver) Version() string { return d.config.Version }
func (d *CamundaDriver) Capabilities() map[Capability]bool {
	return map[Capability]bool{
		CapabilityDeployment: true, CapabilityStart: true, CapabilityTaskQuery: true,
		CapabilityFormRead: true, CapabilityResolvedForm: false, CapabilityTaskSubmit: true,
		CapabilityInstanceQuery: true, CapabilityTimerWait: true,
	}
}

func (d *CamundaDriver) Deploy(ctx context.Context, files []File) (Definition, error) {
	body, contentType, err := DeploymentRequestBody(files)
	if err != nil {
		return Definition{}, err
	}
	response, err := d.request(ctx, http.MethodPost, "/v2/deployments", body, contentType)
	if err != nil {
		return Definition{}, fmt.Errorf("Camunda deployment: %w", err)
	}
	type deployedProcess struct {
		ProcessDefinitionKey     json.RawMessage `json:"processDefinitionKey"`
		ProcessDefinitionID      string          `json:"processDefinitionId"`
		ProcessDefinitionVersion json.RawMessage `json:"processDefinitionVersion"`
	}
	var result struct {
		Deployments []struct {
			ProcessDefinition *deployedProcess `json:"processDefinition"`
			Process           *deployedProcess `json:"process"`
			deployedProcess
		} `json:"deployments"`
	}
	if err := decodeUseNumber(response, &result); err != nil {
		return Definition{}, fmt.Errorf("decode Camunda deployment response: %w", err)
	}
	for _, deployed := range result.Deployments {
		process := deployed.ProcessDefinition
		if process == nil {
			process = deployed.Process
		}
		if process == nil {
			process = &deployed.deployedProcess
		}
		if process.ProcessDefinitionID != "" {
			definition := Definition{
				ProcessKey: process.ProcessDefinitionID, Version: scalarString(process.ProcessDefinitionVersion),
				Handle: scalarString(process.ProcessDefinitionKey),
			}
			d.mu.Lock()
			d.definitions[definition.ProcessKey] = definition
			d.mu.Unlock()
			return definition, nil
		}
	}
	return Definition{}, fmt.Errorf("Camunda deployment did not return a process definition")
}

func DeploymentRequestBody(files []File) ([]byte, string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, file := range files {
		part, err := writer.CreateFormFile("resources", file.Name)
		if err != nil {
			return nil, "", err
		}
		if _, err := io.WriteString(part, file.Content); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return body.Bytes(), writer.FormDataContentType(), nil
}

func (d *CamundaDriver) Start(ctx context.Context, processKey string, variables map[string]any) (Instance, error) {
	definition, ok := d.definition(processKey)
	if !ok {
		definition = Definition{ProcessKey: processKey}
	}
	request := map[string]any{
		"processDefinitionId": processKey,
		"variables":           variables,
		"awaitCompletion":     false,
	}
	if definition.Version != "" {
		if n, err := strconv.Atoi(definition.Version); err == nil {
			request["processDefinitionVersion"] = n
		}
	}
	body, err := json.Marshal(request)
	if err != nil {
		return Instance{}, err
	}
	response, err := d.request(ctx, http.MethodPost, "/v2/process-instances", body, "application/json")
	if err != nil {
		return Instance{}, fmt.Errorf("Camunda start instance: %w", err)
	}
	var result struct {
		ProcessInstanceKey       json.RawMessage `json:"processInstanceKey"`
		ProcessDefinitionKey     json.RawMessage `json:"processDefinitionKey"`
		ProcessDefinitionId      string          `json:"processDefinitionId"`
		ProcessDefinitionVersion json.RawMessage `json:"processDefinitionVersion"`
	}
	if err := decodeUseNumber(response, &result); err != nil {
		return Instance{}, fmt.Errorf("decode Camunda start response: %w", err)
	}
	instanceID := scalarString(result.ProcessInstanceKey)
	if instanceID == "" {
		return Instance{}, fmt.Errorf("Camunda start response omitted processInstanceKey")
	}
	return Instance{
		ID: instanceID, ProcessKey: processKey, Version: scalarString(result.ProcessDefinitionVersion),
		Handle: instanceID,
	}, nil
}

func (d *CamundaDriver) AwaitOpenTasks(ctx context.Context, instance Instance, elementID string, count int) ([]Task, error) {
	var last []Task
	err := d.pollUntil(ctx, func(ctx context.Context) (bool, error) {
		body, err := json.Marshal(map[string]any{
			"filter": map[string]any{"processInstanceKey": instance.ID, "state": "CREATED"},
			"page":   map[string]any{"limit": 100},
		})
		if err != nil {
			return false, err
		}
		response, err := d.request(ctx, http.MethodPost, "/v2/user-tasks/search", body, "application/json")
		if err != nil {
			return false, err
		}
		var result struct {
			Items []struct {
				UserTaskKey        json.RawMessage `json:"userTaskKey"`
				ProcessInstanceKey json.RawMessage `json:"processInstanceKey"`
				ElementID          string          `json:"elementId"`
				DueDate            *time.Time      `json:"dueDate"`
			} `json:"items"`
		}
		if err := decodeUseNumber(response, &result); err != nil {
			return false, fmt.Errorf("decode Camunda task search: %w", err)
		}
		last = last[:0]
		for _, item := range result.Items {
			if elementID == "" || item.ElementID == elementID {
				id := scalarString(item.UserTaskKey)
				last = append(last, Task{
					ID: id, InstanceID: scalarString(item.ProcessInstanceKey),
					ElementID: item.ElementID, DueAt: item.DueDate, Handle: id,
				})
			}
		}
		if count == 0 {
			return len(last) == 0, nil
		}
		return len(last) >= count, nil
	})
	return last, err
}

func (d *CamundaDriver) ReadForm(ctx context.Context, task Task) (any, error) {
	response, err := d.request(ctx, http.MethodGet, "/v2/user-tasks/"+url.PathEscape(task.ID)+"/form", nil, "")
	if err != nil {
		return nil, err
	}
	var result any
	if err := decodeUseNumber(response, &result); err != nil {
		return nil, fmt.Errorf("decode Camunda task form: %w", err)
	}
	return result, nil
}

func (d *CamundaDriver) Submit(ctx context.Context, task Task, fields map[string]string) error {
	body, err := json.Marshal(map[string]any{"variables": fields})
	if err != nil {
		return err
	}
	path := "/v2/user-tasks/" + url.PathEscape(task.ID)
	if _, err := d.request(ctx, http.MethodPost, path+"/completion", body, "application/json"); err != nil {
		return err
	}
	// Task queries are eventually consistent. Don't mistake the just-completed
	// activation for the next open task on a loop or parallel branch.
	err = d.pollUntil(ctx, func(ctx context.Context) (bool, error) {
		data, err := d.request(ctx, http.MethodGet, path, nil, "")
		if err != nil {
			return false, err
		}
		var record struct {
			State string `json:"state"`
		}
		if err := decodeUseNumber(data, &record); err != nil {
			return false, err
		}
		return record.State == "COMPLETED", nil
	})
	if err != nil {
		return fmt.Errorf("observe completion of Camunda task %s: %w", task.ID, err)
	}
	return nil
}

func (d *CamundaDriver) AwaitInstance(ctx context.Context, instance Instance, predicate func(Snapshot) bool) (Snapshot, error) {
	var last Snapshot
	err := d.pollUntil(ctx, func(ctx context.Context) (bool, error) {
		response, err := d.request(ctx, http.MethodGet, "/v2/process-instances/"+url.PathEscape(instance.ID), nil, "")
		if err != nil {
			return false, err
		}
		var record struct {
			State                    string          `json:"state"`
			ProcessDefinitionId      string          `json:"processDefinitionId"`
			ProcessDefinitionVersion json.RawMessage `json:"processDefinitionVersion"`
		}
		if err := decodeUseNumber(response, &record); err != nil {
			return false, fmt.Errorf("decode Camunda process instance: %w", err)
		}
		variables, err := d.readVariables(ctx, instance.ID)
		if err != nil {
			return false, err
		}
		last = Snapshot{
			State: NormalizeState(record.State), Variables: variables,
			Definition: Definition{
				ProcessKey: record.ProcessDefinitionId,
				Version:    scalarString(record.ProcessDefinitionVersion), Handle: record.ProcessDefinitionId,
			},
		}
		return predicate(last), nil
	})
	return last, err
}

func (d *CamundaDriver) readVariables(ctx context.Context, instanceID string) (map[string]any, error) {
	body, err := json.Marshal(map[string]any{"filter": map[string]any{"processInstanceKey": instanceID}, "page": map[string]any{"limit": 500}})
	if err != nil {
		return nil, err
	}
	response, err := d.request(ctx, http.MethodPost, "/v2/variables/search", body, "application/json")
	if err != nil {
		return nil, err
	}
	var result struct {
		Items []struct {
			Name        string `json:"name"`
			Value       string `json:"value"` // REST v2 returns serialized JSON in a string.
			IsTruncated bool   `json:"isTruncated"`
		} `json:"items"`
	}
	if err := decodeUseNumber(response, &result); err != nil {
		return nil, fmt.Errorf("decode Camunda variables: %w", err)
	}
	variables := make(map[string]any, len(result.Items))
	for _, item := range result.Items {
		if item.IsTruncated {
			return nil, fmt.Errorf("Camunda variable %q is truncated", item.Name)
		}
		var value any
		if err := decodeUseNumber([]byte(item.Value), &value); err != nil {
			return nil, fmt.Errorf("decode Camunda variable %q: %w", item.Name, err)
		}
		variables[item.Name] = value
	}
	return variables, nil
}

func (d *CamundaDriver) WaitDeadline(ctx context.Context, task Task) error {
	wait := d.config.TimerWait
	if task.DueAt != nil {
		wait = time.Until(*task.DueAt) + d.poll.QueryLag
	}
	if wait < 0 {
		wait = 0
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (d *CamundaDriver) pollUntil(ctx context.Context, check func(context.Context) (bool, error)) error {
	waitCtx, cancel := context.WithTimeout(ctx, d.poll.Timeout)
	defer cancel()
	for {
		done, err := check(waitCtx)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		timer := time.NewTimer(d.poll.Interval)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			return fmt.Errorf("Camunda query timed out: %w", waitCtx.Err())
		case <-timer.C:
		}
	}
}

func (d *CamundaDriver) request(ctx context.Context, method, path string, body []byte, contentType string) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	requestCtx, cancel := context.WithTimeout(ctx, d.poll.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, method, strings.TrimRight(d.config.Endpoint, "/")+path, reader)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if d.config.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+d.config.BearerToken)
	}
	response, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d %s: %s", response.StatusCode, path, strings.TrimSpace(string(data)))
	}
	return data, nil
}

func (d *CamundaDriver) definition(processKey string) (Definition, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	definition, ok := d.definitions[processKey]
	return definition, ok
}

func scalarString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		return number.String()
	}
	return strings.Trim(string(raw), `"`)
}

func NormalizeState(state string) string {
	switch strings.ToUpper(state) {
	case "COMPLETED", "COMPLETED_STATE", "TERMINATED":
		return "COMPLETED"
	case "ACTIVE", "RUNNING", "CREATED":
		return "RUNNING"
	case "INCIDENT":
		return "INCIDENT"
	default:
		return strings.ToUpper(state)
	}
}
