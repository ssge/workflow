package release1

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

//go:embed flow-form.bpmn form-textfield.form looping_decision.dmn
var fixtureFiles embed.FS

type File struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

func FixtureFiles() ([]File, error) {
	names := []string{"flow-form.bpmn", "form-textfield.form", "looping_decision.dmn"}
	files := make([]File, 0, len(names))
	for _, name := range names {
		content, err := fixtureFiles.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("read fixture %s: %w", name, err)
		}
		files = append(files, File{Name: name, Content: string(content)})
	}
	return files, nil
}

type Definition struct {
	ProcessKey string
	Version    string
	Handle     string
}

type Instance struct {
	ID         string
	ProcessKey string
	Version    string
	Handle     string
}

type Task struct {
	ID         string
	InstanceID string
	ElementID  string
	DueAt      *time.Time
	Handle     string
}

type Snapshot struct {
	State      string
	Variables  map[string]any
	Definition Definition
}

type Capability string

const (
	CapabilityDeployment    Capability = "deployment"
	CapabilityStart         Capability = "start"
	CapabilityTaskQuery     Capability = "task-query"
	CapabilityFormRead      Capability = "form-read"
	CapabilityResolvedForm  Capability = "resolved-form-view"
	CapabilityTaskSubmit    Capability = "task-submit"
	CapabilityInstanceQuery Capability = "instance-query"
	CapabilityTimerWait     Capability = "timer-wait"
	CapabilityClockControl  Capability = "clock-control"
	CapabilityPump          Capability = "pump"
	CapabilityFailpoint     Capability = "failpoint"
)

type Driver interface {
	Backend() string
	Version() string
	Capabilities() map[Capability]bool
	Deploy(context.Context, []File) (Definition, error)
	Start(context.Context, string, map[string]any) (Instance, error)
	// A count of zero waits until no matching open tasks remain.
	AwaitOpenTasks(context.Context, Instance, string, int) ([]Task, error)
	ReadForm(context.Context, Task) (any, error)
	Submit(context.Context, Task, map[string]string) error
	AwaitInstance(context.Context, Instance, func(Snapshot) bool) (Snapshot, error)
	WaitDeadline(context.Context, Task) error
}

// NativeControl deliberately sits outside Driver's public API methods. It is
// only for native-only verification of clock and runner behavior.
type NativeControl interface {
	Now() time.Time
	AdvanceClock(context.Context, time.Duration) error
	Pump(context.Context) error
	InjectFailpoint(context.Context, string) error
}

type Scenario struct {
	Name                 string
	Capabilities         []Capability
	OptionalCapabilities []Capability
	Run                  func(context.Context, Driver) error
}

type ScenarioFailure struct {
	Err       error
	LastState string
	Handles   []string
}

func (e *ScenarioFailure) Error() string { return e.Err.Error() }
func (e *ScenarioFailure) Unwrap() error { return e.Err }

type Status string

const (
	StatusPass        Status = "PASS"
	StatusFail        Status = "FAIL"
	StatusUnsupported Status = "UNSUPPORTED"
	StatusNotRun      Status = "NOT RUN"
)

type Outcome struct {
	Backend    string     `json:"backend"`
	Version    string     `json:"version"`
	Scenario   string     `json:"scenario"`
	Capability Capability `json:"capability"`
	Status     Status     `json:"status"`
	Detail     string     `json:"detail,omitempty"`
	LastState  string     `json:"lastState,omitempty"`
	Handles    []string   `json:"handles,omitempty"`
	Timeout    string     `json:"timeout,omitempty"`
}

type Report struct {
	Outcomes []Outcome `json:"outcomes"`
}

func (r Report) JSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

func RunScenarios(ctx context.Context, driver Driver, scenarios []Scenario) Report {
	report := Report{}
	caps := driver.Capabilities()
	for _, scenario := range scenarios {
		optional := make(map[Capability]bool, len(scenario.OptionalCapabilities))
		for _, capability := range scenario.OptionalCapabilities {
			optional[capability] = true
		}
		unavailable := make([]Capability, 0)
		for _, capability := range scenario.Capabilities {
			if !caps[capability] && !optional[capability] {
				unavailable = append(unavailable, capability)
			}
		}
		if len(unavailable) > 0 {
			for _, capability := range scenario.Capabilities {
				status, detail := StatusNotRun, "scenario requires an unavailable capability"
				if !caps[capability] {
					status, detail = StatusUnsupported, "backend does not expose this capability"
				}
				report.Outcomes = append(report.Outcomes, Outcome{
					Backend: driver.Backend(), Version: driver.Version(),
					Scenario: scenario.Name, Capability: capability, Status: status,
					Detail: detail,
				})
			}
			continue
		}
		err := scenario.Run(ctx, driver)
		for _, capability := range scenario.Capabilities {
			outcome := Outcome{
				Backend: driver.Backend(), Version: driver.Version(),
				Scenario: scenario.Name, Capability: capability, Status: StatusPass,
			}
			if !caps[capability] {
				outcome.Status = StatusUnsupported
				outcome.Detail = "optional assertion is not exposed by this backend"
			} else if err != nil {
				outcome.Status = StatusFail
				outcome.Detail = err.Error()
				outcome.Timeout = timeoutValue(err)
				var failure *ScenarioFailure
				if errors.As(err, &failure) {
					outcome.LastState = failure.LastState
					outcome.Handles = append([]string(nil), failure.Handles...)
				}
			}
			report.Outcomes = append(report.Outcomes, outcome)
		}
	}
	report.Sort()
	return report
}

func (r *Report) Sort() {
	sort.Slice(r.Outcomes, func(i, j int) bool {
		a, b := r.Outcomes[i], r.Outcomes[j]
		if a.Backend != b.Backend {
			return a.Backend < b.Backend
		}
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		if a.Scenario != b.Scenario {
			return a.Scenario < b.Scenario
		}
		return a.Capability < b.Capability
	})
}

func timeoutValue(err error) string {
	if err == nil {
		return ""
	}
	if strings.Contains(strings.ToLower(err.Error()), "deadline") || strings.Contains(strings.ToLower(err.Error()), "timeout") {
		return err.Error()
	}
	return ""
}

func NotConfiguredReport(backend, version string, scenarios []Scenario, reason string) Report {
	report := Report{}
	for _, scenario := range scenarios {
		for _, capability := range scenario.Capabilities {
			report.Outcomes = append(report.Outcomes, Outcome{
				Backend: backend, Version: version, Scenario: scenario.Name,
				Capability: capability, Status: StatusNotRun, Detail: reason,
			})
		}
	}
	report.Sort()
	return report
}

func RunReferenceFromEnv(ctx context.Context, scenarios []Scenario) (Report, error) {
	config, configured, err := CamundaConfigFromEnv()
	if err != nil {
		return Report{}, err
	}
	if !configured {
		return NotConfiguredReport("camunda8", "unconfigured", scenarios, "CAMUNDA8_REST_URL is not set"), nil
	}
	if strings.ToLower(strings.TrimSpace(os.Getenv("CAMUNDA8_ISOLATED"))) != "true" {
		return NotConfiguredReport("camunda8", config.Version, scenarios, "CAMUNDA8_ISOLATED=true is required for the fixed-key fixture"), nil
	}
	driver, err := NewCamundaDriver(config)
	if err != nil {
		return Report{}, err
	}
	return RunScenarios(ctx, driver, scenarios), nil
}

func FixtureFormScenario() Scenario {
	return fixtureScenario("P1-immediate-exit", []Capability{
		CapabilityDeployment, CapabilityStart, CapabilityTaskQuery, CapabilityFormRead,
		CapabilityResolvedForm, CapabilityTaskSubmit, CapabilityInstanceQuery,
	}, []Capability{CapabilityResolvedForm}, runP1)
}

func DefaultScenarios() []Scenario {
	return []Scenario{
		FixtureFormScenario(),
		fixtureScenario("P2-first-beats-stop", fixtureCoreCapabilities(), nil, runP2),
		fixtureScenario("P3-maximum-loop-count", fixtureCoreCapabilities(), nil, runP3),
		fixtureScenario("P4-interrupting-timer", append(fixtureCoreCapabilities(), CapabilityTimerWait), nil, runP4),
		P5ParallelJoinScenario(),
		P6PinnedVersionScenario(),
	}
}

func fixtureCoreCapabilities() []Capability {
	return []Capability{
		CapabilityDeployment, CapabilityStart, CapabilityTaskQuery,
		CapabilityTaskSubmit, CapabilityInstanceQuery,
	}
}

type fixtureExecution struct {
	driver     Driver
	definition Definition
	handles    []string
	lastState  string
}

func fixtureScenario(name string, capabilities, optional []Capability, run func(context.Context, *fixtureExecution) error) Scenario {
	return Scenario{
		Name: name, Capabilities: capabilities, OptionalCapabilities: optional,
		Run: func(ctx context.Context, driver Driver) error {
			execution := &fixtureExecution{driver: driver}
			files, err := FixtureFiles()
			if err != nil {
				return execution.failure(err)
			}
			if err := execution.deploy(ctx, files); err != nil {
				return execution.failure(err)
			}
			if err := run(ctx, execution); err != nil {
				return execution.failure(err)
			}
			return nil
		},
	}
}

func generatedScenario(name string, capabilities []Capability, run func(context.Context, *fixtureExecution) error) Scenario {
	return Scenario{
		Name: name, Capabilities: capabilities,
		Run: func(ctx context.Context, driver Driver) error {
			execution := &fixtureExecution{driver: driver}
			if err := run(ctx, execution); err != nil {
				return execution.failure(err)
			}
			return nil
		},
	}
}

func (e *fixtureExecution) failure(err error) error {
	return &ScenarioFailure{Err: err, LastState: e.lastState, Handles: append([]string(nil), e.handles...)}
}

func (e *fixtureExecution) deploy(ctx context.Context, files []File) error {
	definition, err := e.driver.Deploy(ctx, files)
	if err != nil {
		return fmt.Errorf("deploy model bundle: %w", err)
	}
	e.definition = definition
	e.handles = append(e.handles, definition.Handle)
	return nil
}

func (e *fixtureExecution) start(ctx context.Context, variables map[string]any) (Instance, error) {
	instance, err := e.driver.Start(ctx, e.definition.ProcessKey, variables)
	if err != nil {
		return Instance{}, fmt.Errorf("start fixture: %w", err)
	}
	e.handles = append(e.handles, instance.Handle)
	return instance, nil
}

func (e *fixtureExecution) task(ctx context.Context, instance Instance, count int) (Task, error) {
	tasks, err := e.tasks(ctx, instance, "Activity_1hidco5", count)
	if err != nil {
		return Task{}, err
	}
	return tasks[len(tasks)-1], nil
}

func (e *fixtureExecution) tasks(ctx context.Context, instance Instance, elementID string, count int) ([]Task, error) {
	tasks, err := e.driver.AwaitOpenTasks(ctx, instance, elementID, count)
	for _, task := range tasks {
		e.handles = append(e.handles, task.Handle)
	}
	if err != nil {
		return nil, fmt.Errorf("await open tasks: %w", err)
	}
	if len(tasks) != count {
		return nil, fmt.Errorf("expected exactly %d open tasks, got %d", count, len(tasks))
	}
	return tasks, nil
}

func (e *fixtureExecution) noOpenTasks(ctx context.Context, instance Instance) error {
	return e.noOpenTasksFor(ctx, instance, "Activity_1hidco5")
}

func (e *fixtureExecution) noOpenTasksFor(ctx context.Context, instance Instance, elementID string) error {
	tasks, err := e.driver.AwaitOpenTasks(ctx, instance, elementID, 0)
	if err != nil {
		return fmt.Errorf("await no open fixture tasks: %w", err)
	}
	if len(tasks) != 0 {
		return fmt.Errorf("expected no open tasks, got %d", len(tasks))
	}
	return nil
}

func (e *fixtureExecution) submit(ctx context.Context, task Task, fields map[string]string) error {
	if err := e.driver.Submit(ctx, task, fields); err != nil {
		return fmt.Errorf("submit task %s: %w", task.ID, err)
	}
	return nil
}

func (e *fixtureExecution) await(ctx context.Context, instance Instance, predicate func(Snapshot) bool) (Snapshot, error) {
	snapshot, err := e.driver.AwaitInstance(ctx, instance, func(observed Snapshot) bool {
		e.lastState = observed.State
		return predicate(observed)
	})
	if err != nil {
		e.lastState = snapshot.State
		return snapshot, fmt.Errorf("await instance (last state %q): %w", snapshot.State, err)
	}
	e.lastState = snapshot.State
	return snapshot, nil
}

func runP1(ctx context.Context, e *fixtureExecution) error {
	for _, field := range []string{"text_command", "command"} {
		instance, err := e.start(ctx, map[string]any{"input": "hello"})
		if err != nil {
			return err
		}
		task, err := e.task(ctx, instance, 1)
		if err != nil {
			return err
		}
		form, err := e.driver.ReadForm(ctx, task)
		if err != nil {
			return fmt.Errorf("read task form: %w", err)
		}
		if e.driver.Capabilities()[CapabilityResolvedForm] {
			formJSON, _ := json.Marshal(form)
			if !strings.Contains(string(formJSON), "Caller says: hello") {
				return fmt.Errorf("resolved form does not contain expected input text: %s", formJSON)
			}
		}
		if err := e.submit(ctx, task, map[string]string{field: "terminate"}); err != nil {
			return err
		}
		final, err := e.await(ctx, instance, func(snapshot Snapshot) bool { return snapshot.State == "COMPLETED" })
		if err != nil {
			return err
		}
		if err := expectResult(final, 1, "break"); err != nil {
			return err
		}
		if err := e.noOpenTasks(ctx, instance); err != nil {
			return err
		}
	}
	return nil
}

func runP2(ctx context.Context, e *fixtureExecution) error {
	instance, err := e.start(ctx, map[string]any{"input": "hello"})
	if err != nil {
		return err
	}
	task, err := e.task(ctx, instance, 1)
	if err != nil {
		return err
	}
	previousTask := task.ID
	for count := 1; count <= 2; count++ {
		if err := e.submit(ctx, task, map[string]string{"command": "stop"}); err != nil {
			return err
		}
		task, err = e.task(ctx, instance, 1)
		if err != nil {
			return err
		}
		if task.ID == previousTask {
			return fmt.Errorf("loop activation reused task handle %q", task.ID)
		}
		previousTask = task.ID
		snapshot, err := e.await(ctx, instance, func(snapshot Snapshot) bool {
			return snapshot.State == "RUNNING" && numberEquals(snapshot.Variables["executionCount"], count) &&
				snapshot.Variables["decision"] == "loop"
		})
		if err != nil {
			return err
		}
		if err := expectResult(snapshot, count, "loop"); err != nil {
			return err
		}
	}
	if err := e.submit(ctx, task, map[string]string{"command": "stop"}); err != nil {
		return err
	}
	final, err := e.await(ctx, instance, func(snapshot Snapshot) bool { return snapshot.State == "COMPLETED" })
	if err != nil {
		return err
	}
	if err := expectResult(final, 3, "break"); err != nil {
		return err
	}
	return e.noOpenTasks(ctx, instance)
}

func runP3(ctx context.Context, e *fixtureExecution) error {
	instance, err := e.start(ctx, map[string]any{"input": "hello"})
	if err != nil {
		return err
	}
	task, err := e.task(ctx, instance, 1)
	if err != nil {
		return err
	}
	previousTask := task.ID
	for count := 1; count <= 11; count++ {
		if err := e.submit(ctx, task, map[string]string{"command": "continue"}); err != nil {
			return err
		}
		if count == 11 {
			break
		}
		task, err = e.task(ctx, instance, 1)
		if err != nil {
			return err
		}
		if task.ID == previousTask {
			return fmt.Errorf("loop activation reused task handle %q", task.ID)
		}
		previousTask = task.ID
	}
	final, err := e.await(ctx, instance, func(snapshot Snapshot) bool { return snapshot.State == "COMPLETED" })
	if err != nil {
		return err
	}
	if err := expectResult(final, 11, "break"); err != nil {
		return err
	}
	return e.noOpenTasks(ctx, instance)
}

func runP4(ctx context.Context, e *fixtureExecution) error {
	instance, err := e.start(ctx, map[string]any{"input": "hello", "command": "continue", "text_command": ""})
	if err != nil {
		return err
	}
	oldTask, err := e.task(ctx, instance, 1)
	if err != nil {
		return err
	}
	if err := e.driver.WaitDeadline(ctx, oldTask); err != nil {
		return fmt.Errorf("wait for interrupting timer deadline: %w", err)
	}
	currentTask, err := e.task(ctx, instance, 1)
	if err != nil {
		return err
	}
	if currentTask.ID == oldTask.ID {
		return fmt.Errorf("timer did not replace old task handle %q", oldTask.ID)
	}
	if err := e.submit(ctx, oldTask, map[string]string{"text_command": "terminate"}); err == nil {
		return fmt.Errorf("late completion on old task %q was accepted", oldTask.ID)
	}
	if err := e.submit(ctx, currentTask, map[string]string{"text_command": "terminate"}); err != nil {
		return err
	}
	final, err := e.await(ctx, instance, func(snapshot Snapshot) bool { return snapshot.State == "COMPLETED" })
	if err != nil {
		return err
	}
	if err := expectResult(final, 2, "break"); err != nil {
		return err
	}
	return e.noOpenTasks(ctx, instance)
}

func expectResult(snapshot Snapshot, count int, decision string) error {
	if !numberEquals(snapshot.Variables["executionCount"], count) {
		return fmt.Errorf("expected executionCount %d, got %v", count, snapshot.Variables["executionCount"])
	}
	if snapshot.Variables["decision"] != decision {
		return fmt.Errorf("expected decision %s, got %v", decision, snapshot.Variables["decision"])
	}
	return nil
}

func numberEquals(value any, want int) bool {
	switch number := value.(type) {
	case json.Number:
		return number.String() == fmt.Sprint(want)
	case float64:
		return number == float64(want)
	case int:
		return number == want
	default:
		return false
	}
}
