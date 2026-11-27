package release1

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type PollConfig struct {
	Interval time.Duration
	Timeout  time.Duration
	QueryLag time.Duration
}

func (c PollConfig) normalized() PollConfig {
	if c.Interval <= 0 {
		c.Interval = 100 * time.Millisecond
	}
	if c.Timeout <= 0 {
		c.Timeout = 75 * time.Second
	}
	if c.QueryLag <= 0 {
		c.QueryLag = c.Interval
	}
	return c
}

type NativeDriver struct {
	baseURL string
	client  *http.Client
	control NativeControl
	poll    PollConfig
	version string
}

type nativeOperation struct {
	OperationID string          `json:"operationId"`
	Status      string          `json:"status"`
	Result      json.RawMessage `json:"result"`
	Error       *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func NewNativeDriver(baseURL string, client *http.Client, version string, control NativeControl, poll PollConfig) (*NativeDriver, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid native API URL %q", baseURL)
	}
	if client == nil {
		client = http.DefaultClient
	}
	if version == "" {
		version = "unknown"
	}
	return &NativeDriver{
		baseURL: strings.TrimRight(baseURL, "/"), client: client, control: control,
		poll: poll.normalized(), version: version,
	}, nil
}

func (d *NativeDriver) Backend() string { return "native" }
func (d *NativeDriver) Version() string { return d.version }

func (d *NativeDriver) Capabilities() map[Capability]bool {
	result := map[Capability]bool{
		CapabilityDeployment: true, CapabilityStart: true, CapabilityTaskQuery: true,
		CapabilityFormRead: true, CapabilityResolvedForm: true,
		CapabilityTaskSubmit: true, CapabilityInstanceQuery: true,
		CapabilityTimerWait: true,
	}
	if d.control != nil {
		result[CapabilityClockControl] = true
		result[CapabilityPump] = true
		result[CapabilityFailpoint] = true
	}
	return result
}

func (d *NativeDriver) Control() (NativeControl, bool) {
	return d.control, d.control != nil
}

func (d *NativeDriver) Deploy(ctx context.Context, files []File) (Definition, error) {
	payload, err := json.Marshal(struct {
		Files []File `json:"files"`
	}{Files: files})
	if err != nil {
		return Definition{}, err
	}
	operation, err := d.command(ctx, http.MethodPost, "/v1/deployments", payload)
	if err != nil {
		return Definition{}, err
	}
	var result struct {
		ProcessKey string `json:"processKey"`
		Version    any    `json:"version"`
	}
	if err := json.Unmarshal(operation.Result, &result); err != nil {
		return Definition{}, fmt.Errorf("decode deployment result: %w", err)
	}
	if result.ProcessKey == "" {
		return Definition{}, fmt.Errorf("deployment result omitted processKey")
	}
	return Definition{ProcessKey: result.ProcessKey, Version: versionString(result.Version), Handle: result.ProcessKey}, nil
}

func (d *NativeDriver) Start(ctx context.Context, processKey string, variables map[string]any) (Instance, error) {
	if variables == nil {
		variables = map[string]any{}
	}
	payload, err := json.Marshal(struct {
		ProcessKey string         `json:"processKey"`
		Variables  map[string]any `json:"variables"`
	}{ProcessKey: processKey, Variables: variables})
	if err != nil {
		return Instance{}, err
	}
	operation, err := d.command(ctx, http.MethodPost, "/v1/instances", payload)
	if err != nil {
		return Instance{}, err
	}
	var result struct {
		InstanceID string `json:"instanceId"`
		ProcessKey string `json:"processKey"`
		Version    any    `json:"version"`
	}
	if err := json.Unmarshal(operation.Result, &result); err != nil {
		return Instance{}, fmt.Errorf("decode start result: %w", err)
	}
	if result.InstanceID == "" {
		return Instance{}, fmt.Errorf("start result omitted instanceId")
	}
	return Instance{
		ID: result.InstanceID, ProcessKey: result.ProcessKey,
		Version: versionString(result.Version), Handle: result.InstanceID,
	}, nil
}

func (d *NativeDriver) AwaitOpenTasks(ctx context.Context, instance Instance, elementID string, count int) ([]Task, error) {
	var last []Task
	err := d.pollUntil(ctx, func(ctx context.Context) (bool, error) {
		path := "/v1/tasks?instanceId=" + url.QueryEscape(instance.ID) + "&status=OPEN"
		var page struct {
			Items []struct {
				TaskID     string     `json:"taskId"`
				InstanceID string     `json:"instanceId"`
				ElementID  string     `json:"elementId"`
				DueAt      *time.Time `json:"dueAt"`
			} `json:"items"`
		}
		if err := d.getJSON(ctx, path, &page); err != nil {
			return false, err
		}
		last = last[:0]
		for _, item := range page.Items {
			if elementID == "" || item.ElementID == elementID {
				last = append(last, Task{ID: item.TaskID, InstanceID: item.InstanceID, ElementID: item.ElementID, DueAt: item.DueAt, Handle: item.TaskID})
			}
		}
		if count == 0 {
			return len(last) == 0, nil
		}
		return len(last) >= count, nil
	})
	if err != nil {
		return last, err
	}
	return last, nil
}

func (d *NativeDriver) ReadForm(ctx context.Context, task Task) (any, error) {
	var result struct {
		Components json.RawMessage `json:"components"`
	}
	if err := d.getJSON(ctx, "/v1/tasks/"+url.PathEscape(task.ID), &result); err != nil {
		return nil, err
	}
	var decoded any
	if err := decodeUseNumber(result.Components, &decoded); err != nil {
		return nil, fmt.Errorf("decode task components: %w", err)
	}
	return decoded, nil
}

func (d *NativeDriver) Submit(ctx context.Context, task Task, fields map[string]string) error {
	if fields == nil {
		fields = map[string]string{}
	}
	payload, err := json.Marshal(struct {
		Fields map[string]string `json:"fields"`
	}{Fields: fields})
	if err != nil {
		return err
	}
	_, err = d.command(ctx, http.MethodPost, "/v1/tasks/"+url.PathEscape(task.ID)+"/complete", payload)
	return err
}

func (d *NativeDriver) AwaitInstance(ctx context.Context, instance Instance, predicate func(Snapshot) bool) (Snapshot, error) {
	var last Snapshot
	err := d.pollUntil(ctx, func(ctx context.Context) (bool, error) {
		var response struct {
			Status     string          `json:"status"`
			ProcessKey string          `json:"processKey"`
			Version    any             `json:"version"`
			Variables  json.RawMessage `json:"variables"`
		}
		if err := d.getJSON(ctx, "/v1/instances/"+url.PathEscape(instance.ID), &response); err != nil {
			return false, err
		}
		variables := map[string]any{}
		if len(response.Variables) > 0 && string(response.Variables) != "null" {
			if err := decodeUseNumber(response.Variables, &variables); err != nil {
				return false, fmt.Errorf("decode instance variables: %w", err)
			}
		}
		last = Snapshot{
			State: NormalizeState(response.Status), Variables: variables,
			Definition: Definition{ProcessKey: response.ProcessKey, Version: versionString(response.Version), Handle: response.ProcessKey},
		}
		return predicate(last), nil
	})
	return last, err
}

func (d *NativeDriver) WaitDeadline(ctx context.Context, task Task) error {
	if task.DueAt == nil {
		return fmt.Errorf("task has no due time")
	}
	if d.control != nil {
		delta := task.DueAt.Sub(d.control.Now())
		if delta > 0 {
			if err := d.control.AdvanceClock(ctx, delta); err != nil {
				return err
			}
		}
		return d.control.Pump(ctx)
	}

	target := task.DueAt.Add(d.poll.QueryLag)
	wait := time.Until(target)
	if wait <= 0 {
		return nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, d.poll.Timeout)
	defer cancel()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		if err := waitCtx.Err(); err != nil {
			return fmt.Errorf("native real-time deadline wait timed out: %w", err)
		}
		return nil
	case <-waitCtx.Done():
		return fmt.Errorf("native real-time deadline wait timed out (dueAt %s plus query lag %s): %w",
			task.DueAt.UTC().Format(time.RFC3339Nano), d.poll.QueryLag, waitCtx.Err())
	}
}

func (d *NativeDriver) command(ctx context.Context, method, path string, body []byte) (nativeOperation, error) {
	var receipt nativeOperation
	key, err := randomCommandKey()
	if err != nil {
		return receipt, err
	}
	response, err := d.request(ctx, method, path, body, key)
	if err != nil {
		return receipt, err
	}
	if err := json.Unmarshal(response, &receipt); err != nil {
		return receipt, fmt.Errorf("decode command receipt: %w", err)
	}
	if receipt.OperationID == "" {
		return receipt, fmt.Errorf("command receipt omitted operationId")
	}
	return d.awaitOperation(ctx, receipt)
}

func (d *NativeDriver) awaitOperation(ctx context.Context, operation nativeOperation) (nativeOperation, error) {
	last := operation
	last.Status = ""
	err := d.pollUntil(ctx, func(ctx context.Context) (bool, error) {
		if last.Status == "SUCCEEDED" || last.Status == "FAILED" {
			return true, nil
		}
		if err := d.getJSON(ctx, "/v1/operations/"+url.PathEscape(operation.OperationID), &last); err != nil {
			return false, err
		}
		return last.Status == "SUCCEEDED" || last.Status == "FAILED", nil
	})
	if err != nil {
		return last, err
	}
	if last.Status == "FAILED" {
		if last.Error == nil {
			return last, fmt.Errorf("operation %s failed without an error", operation.OperationID)
		}
		return last, fmt.Errorf("operation %s failed: %s: %s", operation.OperationID, last.Error.Code, last.Error.Message)
	}
	return last, nil
}

func (d *NativeDriver) pollUntil(ctx context.Context, check func(context.Context) (bool, error)) error {
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
			return fmt.Errorf("native query timed out: %w", waitCtx.Err())
		case <-timer.C:
		}
	}
}

func (d *NativeDriver) getJSON(ctx context.Context, path string, target any) error {
	body, err := d.request(ctx, http.MethodGet, path, nil, "")
	if err != nil {
		return err
	}
	if err := decodeUseNumber(body, target); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func (d *NativeDriver) request(ctx context.Context, method, path string, body []byte, key string) ([]byte, error) {
	var requestBody io.Reader
	if body != nil {
		requestBody = bytes.NewReader(body)
	}
	requestCtx, cancel := context.WithTimeout(ctx, d.poll.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, method, d.baseURL+path, requestBody)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	response, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d %s: %s", resp.StatusCode, path, strings.TrimSpace(string(response)))
	}
	return response, nil
}

func randomCommandKey() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate idempotency key: %w", err)
	}
	return "acceptance-" + hex.EncodeToString(bytes[:]), nil
}

func UniqueProcessKey() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate process key: %w", err)
	}
	return "Acceptance_" + hex.EncodeToString(bytes[:]), nil
}

func decodeUseNumber(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return decoder.Decode(target)
}

func versionString(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}
