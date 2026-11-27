package release1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ssge/workflow/internal/model"
)

func TestCamundaTranslationAcrossPublicAPIs(t *testing.T) {
	fixtures, err := FixtureFiles()
	if err != nil {
		t.Fatal(err)
	}
	var checked []string
	var completionReads int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checked = append(checked, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/deployments":
			mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mediaType != "multipart/form-data" {
				t.Errorf("deployment content type = %q, err=%v", r.Header.Get("Content-Type"), err)
			}
			reader := multipart.NewReader(r.Body, params["boundary"])
			found := map[string]string{}
			for {
				part, err := reader.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Errorf("read multipart part: %v", err)
					break
				}
				content, _ := io.ReadAll(part)
				found[part.FileName()] = string(content)
			}
			if len(found) != len(fixtures) {
				t.Errorf("deployment sent %d files, want %d", len(found), len(fixtures))
			}
			for _, fixture := range fixtures {
				if found[fixture.Name] != fixture.Content {
					t.Errorf("fixture %s bytes were changed in translation", fixture.Name)
				}
			}
			writeJSONTest(w, `{"deployments":[{"process":{"processDefinitionKey":"123","processDefinitionId":"Process_Collect_from_Form","processDefinitionVersion":7}}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v2/process-instances":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["processDefinitionId"] != "Process_Collect_from_Form" || body["processDefinitionVersion"] != float64(7) {
				t.Errorf("start request did not pin the deployed definition: %#v", body)
			}
			if body["awaitCompletion"] != false || body["variables"].(map[string]any)["input"] != "hello" {
				t.Errorf("start payload translation mismatch: %#v", body)
			}
			writeJSONTest(w, `{"processInstanceKey":"456","processDefinitionVersion":7}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v2/user-tasks/search":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			filter := body["filter"].(map[string]any)
			if filter["processInstanceKey"] != "456" || filter["state"] != "CREATED" {
				t.Errorf("task query filters mismatch: %#v", filter)
			}
			writeJSONTest(w, `{"items":[{"userTaskKey":"789","processInstanceKey":"456","elementId":"Activity_1hidco5"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v2/user-tasks/789/form":
			writeJSONTest(w, `{"form":{"components":[]}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v2/user-tasks/789/completion":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["variables"].(map[string]any)["text_command"] != "terminate" {
				t.Errorf("completion fields were not translated to variables: %#v", body)
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/v2/user-tasks/789":
			completionReads++
			if completionReads == 1 {
				writeJSONTest(w, `{"state":"CREATED"}`)
			} else {
				writeJSONTest(w, `{"state":"COMPLETED"}`)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/v2/process-instances/456":
			writeJSONTest(w, `{"state":"COMPLETED","processDefinitionId":"Process_Collect_from_Form","processDefinitionVersion":7}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v2/variables/search":
			writeJSONTest(w, `{"items":[{"name":"executionCount","value":"1"},{"name":"decision","value":"\"break\""}]}`)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	driver, err := NewCamundaDriver(CamundaConfig{
		Endpoint: server.URL, Version: "test-8", Client: server.Client(),
		Poll: PollConfig{Interval: time.Millisecond, Timeout: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	definition, err := driver.Deploy(ctx, fixtures)
	if err != nil {
		t.Fatal(err)
	}
	if definition.ProcessKey != "Process_Collect_from_Form" || definition.Version != "7" {
		t.Fatalf("unexpected normalized definition: %+v", definition)
	}
	instance, err := driver.Start(ctx, definition.ProcessKey, map[string]any{"input": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if instance.ID != "456" || instance.Version != "7" {
		t.Fatalf("unexpected normalized instance: %+v", instance)
	}
	tasks, err := driver.AwaitOpenTasks(ctx, instance, "Activity_1hidco5", 1)
	if err != nil || len(tasks) != 1 || tasks[0].ID != "789" {
		t.Fatalf("task query result = %+v, err=%v", tasks, err)
	}
	if _, err := driver.ReadForm(ctx, tasks[0]); err != nil {
		t.Fatal(err)
	}
	if err := driver.Submit(ctx, tasks[0], map[string]string{"text_command": "terminate"}); err != nil {
		t.Fatal(err)
	}
	final, err := driver.AwaitInstance(ctx, instance, func(snapshot Snapshot) bool {
		return snapshot.State == "COMPLETED"
	})
	if err != nil {
		t.Fatal(err)
	}
	if final.Variables["decision"] != "break" || final.Variables["executionCount"] != json.Number("1") {
		t.Fatalf("unexpected normalized final instance: %+v", final)
	}
	want := []string{
		"POST /v2/deployments", "POST /v2/process-instances", "POST /v2/user-tasks/search",
		"GET /v2/user-tasks/789/form", "POST /v2/user-tasks/789/completion",
		"GET /v2/user-tasks/789", "GET /v2/user-tasks/789",
		"GET /v2/process-instances/456", "POST /v2/variables/search",
	}
	if fmt.Sprint(checked) != fmt.Sprint(want) {
		t.Fatalf("translated API calls = %v, want %v", checked, want)
	}
}

func TestCamundaDeployMixedResources(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONTest(w, `{"deploymentKey":"2251799813775818","deployments":[
			{"processDefinition":null,"form":{"formId":"Form_0w2uzn0","version":7}},
			{"processDefinition":{"processDefinitionId":"Process_Collect_from_Form","processDefinitionVersion":9,"processDefinitionKey":"2251799813749836"},"form":null}
		]}`)
	}))
	defer server.Close()
	driver, err := NewCamundaDriver(CamundaConfig{Endpoint: server.URL, Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	files, err := FixtureFiles()
	if err != nil {
		t.Fatal(err)
	}
	got, err := driver.Deploy(context.Background(), files)
	if err != nil {
		t.Fatal(err)
	}
	want := Definition{ProcessKey: "Process_Collect_from_Form", Version: "9", Handle: "2251799813749836"}
	if got != want {
		t.Fatalf("deployed definition = %+v, want %+v", got, want)
	}
}

func TestDeploymentRequestBodyPreservesFileBytes(t *testing.T) {
	input := []File{{Name: "model.bpmn", Content: "<xml>\r\n exact bytes \x00 </xml>"}}
	body, contentType, err := DeploymentRequestBody(input)
	if err != nil {
		t.Fatal(err)
	}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatal(err)
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	part, err := reader.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(part)
	if err != nil {
		t.Fatal(err)
	}
	if part.FormName() != "resources" || part.FileName() != input[0].Name || string(got) != input[0].Content {
		t.Fatalf("multipart translation changed file: name=%q filename=%q content=%q", part.FormName(), part.FileName(), got)
	}
}

func TestStateNormalization(t *testing.T) {
	cases := map[string]string{
		"ACTIVE": "RUNNING", "RUNNING": "RUNNING", "COMPLETED": "COMPLETED",
		"TERMINATED": "COMPLETED", "incident": "INCIDENT", "unknown-state": "UNKNOWN-STATE",
	}
	for input, want := range cases {
		if got := NormalizeState(input); got != want {
			t.Errorf("NormalizeState(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestGeneratedProcessKeysAreUnique(t *testing.T) {
	first, err := UniqueProcessKey()
	if err != nil {
		t.Fatal(err)
	}
	second, err := UniqueProcessKey()
	if err != nil {
		t.Fatal(err)
	}
	if first == second || !strings.HasPrefix(first, "Acceptance_") || !strings.HasPrefix(second, "Acceptance_") {
		t.Fatalf("generated process keys are not isolated: %q, %q", first, second)
	}
}

func TestP5P6AreRegisteredAsPortableScenarios(t *testing.T) {
	scenarios := DefaultScenarios()
	names := map[string]Scenario{}
	for _, scenario := range scenarios {
		names[scenario.Name] = scenario
	}
	for _, name := range []string{"P5-parallel-join", "P6-pinned-version-path"} {
		scenario, ok := names[name]
		if !ok || scenario.Run == nil {
			t.Fatalf("%s is not registered in the built-in suite", name)
		}
		for _, capability := range scenario.Capabilities {
			if capability == CapabilityClockControl || capability == CapabilityPump || capability == CapabilityFailpoint {
				t.Fatalf("%s incorrectly depends on native-only control %q", name, capability)
			}
		}
	}
}

func TestGeneratedP5AndP6Bundles(t *testing.T) {
	processKey, err := UniqueProcessKey()
	if err != nil {
		t.Fatal(err)
	}
	p5, err := parallelJoinFiles(processKey)
	if err != nil {
		t.Fatal(err)
	}
	p5ID, ok := generatedProcessID(p5)
	if !ok || p5ID != processKey {
		t.Fatalf("P5 process id = %q, want %q", p5ID, processKey)
	}
	p5BPMN := p5[0].Content
	for _, element := range []string{p5LeftElement, p5RightElement, "<bpmn:parallelGateway id=\"Fork\"", "<bpmn:parallelGateway id=\"Join\"", "joined + 1"} {
		if !strings.Contains(p5BPMN, element) {
			t.Errorf("P5 bundle is missing %q", element)
		}
	}
	if len(p5) != 3 || !strings.Contains(p5[1].Content, `"key":"left"`) ||
		!strings.Contains(p5[2].Content, `"key":"right"`) {
		t.Fatalf("P5 branch forms are incomplete: %+v", p5)
	}
	compileGeneratedBundle(t, p5)

	v1, v2, err := pinnedVersionFiles(processKey)
	if err != nil {
		t.Fatal(err)
	}
	v1ID, ok1 := generatedProcessID(v1)
	v2ID, ok2 := generatedProcessID(v2)
	if !ok1 || !ok2 || v1ID != processKey || v2ID != processKey {
		t.Fatalf("P6 bundles must share the generated process key: v1=%q v2=%q", v1ID, v2ID)
	}
	if v1[1].Content != v2[1].Content {
		t.Fatal("P6 v1/v2 form sources must remain identical")
	}
	if !strings.Contains(v1[0].Content, `&quot;old&quot;`) ||
		!strings.Contains(v2[0].Content, `&quot;new&quot;`) ||
		!strings.Contains(v1[0].Content, p6TaskElement) ||
		!strings.Contains(v2[0].Content, p6TaskElement) {
		t.Fatal("P6 bundles do not encode distinct pinned script outputs")
	}
	compileGeneratedBundle(t, v1)
	compileGeneratedBundle(t, v2)
}

func compileGeneratedBundle(t *testing.T, files []File) {
	t.Helper()
	sources := make([]model.Source, 0, len(files))
	for _, file := range files {
		sources = append(sources, model.NewSource(file.Name, file.Content))
	}
	if _, diagnostics := (model.Compiler{}).Compile(sources); len(diagnostics) > 0 {
		t.Fatalf("generated process bundle is rejected: %+v", diagnostics)
	}
}

func TestNativeDriverUsesOnlyPublicHTTPAPI(t *testing.T) {
	fixtures, err := FixtureFiles()
	if err != nil {
		t.Fatal(err)
	}
	var checked []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checked = append(checked, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/deployments":
			if r.Header.Get("Idempotency-Key") == "" {
				t.Error("deployment missing idempotency key")
			}
			var body struct {
				Files []File `json:"files"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if fmt.Sprint(body.Files) != fmt.Sprint(fixtures) {
				t.Errorf("native deploy changed fixture content")
			}
			writeJSONTest(w, `{"operationId":"deploy-op","status":"PENDING"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/operations/deploy-op":
			writeJSONTest(w, `{"operationId":"deploy-op","status":"SUCCEEDED","result":{"processKey":"Process_Collect_from_Form","version":1}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/instances":
			if r.Header.Get("Idempotency-Key") == "" {
				t.Error("start missing idempotency key")
			}
			writeJSONTest(w, `{"operationId":"start-op","status":"PENDING"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/operations/start-op":
			writeJSONTest(w, `{"operationId":"start-op","status":"SUCCEEDED","result":{"instanceId":"i-1","processKey":"Process_Collect_from_Form","version":1}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks":
			writeJSONTest(w, `{"items":[{"taskId":"t-1","instanceId":"i-1","elementId":"Activity_1hidco5","dueAt":null}],"nextCursor":null}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/t-1":
			writeJSONTest(w, `{"components":[{"text":"Caller says: hello"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tasks/t-1/complete":
			if r.Header.Get("Idempotency-Key") == "" {
				t.Error("task completion missing idempotency key")
			}
			writeJSONTest(w, `{"operationId":"complete-op","status":"PENDING"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/operations/complete-op":
			writeJSONTest(w, `{"operationId":"complete-op","status":"SUCCEEDED","result":{"taskId":"t-1"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/instances/i-1":
			writeJSONTest(w, `{"instanceId":"i-1","processKey":"Process_Collect_from_Form","version":1,"status":"COMPLETED","variables":{"executionCount":1,"decision":"break"}}`)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	driver, err := NewNativeDriver(server.URL, server.Client(), "native-test", nil,
		PollConfig{Interval: time.Millisecond, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	definition, err := driver.Deploy(ctx, fixtures)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := driver.Start(ctx, definition.ProcessKey, map[string]any{"input": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := driver.AwaitOpenTasks(ctx, instance, "Activity_1hidco5", 1)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("task query result = %+v, err=%v", tasks, err)
	}
	form, err := driver.ReadForm(ctx, tasks[0])
	if err != nil || !strings.Contains(fmt.Sprint(form), "Caller says: hello") {
		t.Fatalf("form read = %v, err=%v", form, err)
	}
	if err := driver.Submit(ctx, tasks[0], map[string]string{"text_command": "terminate"}); err != nil {
		t.Fatal(err)
	}
	final, err := driver.AwaitInstance(ctx, instance, func(snapshot Snapshot) bool {
		return snapshot.State == "COMPLETED"
	})
	if err != nil || final.Variables["decision"] != "break" {
		t.Fatalf("final instance = %+v, err=%v", final, err)
	}
	want := []string{
		"POST /v1/deployments", "GET /v1/operations/deploy-op",
		"POST /v1/instances", "GET /v1/operations/start-op", "GET /v1/tasks",
		"GET /v1/tasks/t-1", "POST /v1/tasks/t-1/complete", "GET /v1/operations/complete-op",
		"GET /v1/instances/i-1",
	}
	if fmt.Sprint(checked) != fmt.Sprint(want) {
		t.Fatalf("native API calls = %v, want %v", checked, want)
	}
	if !driver.Capabilities()[CapabilityResolvedForm] {
		t.Fatal("native driver should expose resolved task form views")
	}
}

func TestNativeOnlyControlIsSeparateAndSupportsClockPumpFailpoint(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	control := &fakeControl{now: now}
	driver, err := NewNativeDriver("http://127.0.0.1:8080", nil, "test", control, PollConfig{})
	if err != nil {
		t.Fatal(err)
	}
	due := now.Add(30 * time.Second)
	if err := driver.WaitDeadline(context.Background(), Task{DueAt: &due}); err != nil {
		t.Fatal(err)
	}
	if control.advanced != 30*time.Second || control.pumps != 1 {
		t.Fatalf("clock/pump calls: advanced=%s pumps=%d", control.advanced, control.pumps)
	}
	got, ok := driver.Control()
	if !ok {
		t.Fatal("native-only control interface missing")
	}
	if err := got.InjectFailpoint(context.Background(), "commit-before-write"); err != nil {
		t.Fatal(err)
	}
	if control.failpoint != "commit-before-write" {
		t.Fatalf("failpoint = %q", control.failpoint)
	}
	caps := driver.Capabilities()
	if !caps[CapabilityClockControl] || !caps[CapabilityPump] || !caps[CapabilityFailpoint] {
		t.Fatalf("native control capabilities missing: %#v", caps)
	}
}

func TestNativeRealTimeDeadlineWaitUsesDueAtAndBoundsTimeout(t *testing.T) {
	driver, err := NewNativeDriver("http://127.0.0.1:8080", nil, "test", nil,
		PollConfig{Interval: time.Millisecond, Timeout: time.Second, QueryLag: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if !driver.Capabilities()[CapabilityTimerWait] {
		t.Fatal("native timer wait should be available without control hooks")
	}
	if _, ok := driver.Control(); ok {
		t.Fatal("real-time timer wait unexpectedly requires native control")
	}

	due := time.Now().Add(45 * time.Millisecond)
	target := due.Add(20 * time.Millisecond)
	started := time.Now()
	if err := driver.WaitDeadline(context.Background(), Task{DueAt: &due}); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	if elapsed+10*time.Millisecond < target.Sub(started) {
		t.Fatalf("returned before dueAt plus query lag: elapsed=%s target=%s", elapsed, target.Sub(started))
	}

	timeoutDriver, err := NewNativeDriver("http://127.0.0.1:8080", nil, "test", nil,
		PollConfig{Timeout: 25 * time.Millisecond, QueryLag: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Second)
	started = time.Now()
	err = timeoutDriver.WaitDeadline(context.Background(), Task{DueAt: &future})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected bounded deadline timeout, got %v", err)
	}
	if elapsed = time.Since(started); elapsed > 300*time.Millisecond {
		t.Fatalf("real-time timer wait exceeded configured bound: %s", elapsed)
	}

	parentDriver, err := NewNativeDriver("http://127.0.0.1:8080", nil, "test", nil,
		PollConfig{Timeout: time.Second, QueryLag: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	parent, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	future = time.Now().Add(time.Second)
	err = parentDriver.WaitDeadline(parent, Task{DueAt: &future})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("parent context deadline was not respected: %v", err)
	}
}

func TestReportStatusesAndOptionalCapabilities(t *testing.T) {
	scenarios := []Scenario{
		{
			Name: "reporting", Capabilities: []Capability{"supported", "optional"},
			OptionalCapabilities: []Capability{"optional"},
			Run:                  func(context.Context, Driver) error { return nil },
		},
		{
			Name: "missing", Capabilities: []Capability{"missing", "not-run"},
			Run: func(context.Context, Driver) error { t.Fatal("scenario ran without required capability"); return nil },
		},
	}
	report := RunScenarios(context.Background(), fakeDriver{caps: map[Capability]bool{"supported": true, "not-run": true}}, scenarios)
	statuses := map[Capability]Status{}
	for _, outcome := range report.Outcomes {
		statuses[outcome.Capability] = outcome.Status
	}
	if statuses["supported"] != StatusPass || statuses["optional"] != StatusUnsupported ||
		statuses["missing"] != StatusUnsupported || statuses["not-run"] != StatusNotRun {
		t.Fatalf("unexpected capability statuses: %#v", statuses)
	}
	failed := RunScenarios(context.Background(), fakeDriver{caps: map[Capability]bool{"supported": true}}, []Scenario{{
		Name: "failure", Capabilities: []Capability{"supported"},
		Run: func(context.Context, Driver) error {
			return &ScenarioFailure{
				Err: fmt.Errorf("observable divergence"), LastState: "RUNNING",
				Handles: []string{"deployment-1", "instance-1"},
			}
		},
	}})
	if len(failed.Outcomes) != 1 || failed.Outcomes[0].Status != StatusFail ||
		failed.Outcomes[0].Detail != "observable divergence" || failed.Outcomes[0].LastState != "RUNNING" ||
		fmt.Sprint(failed.Outcomes[0].Handles) != "[deployment-1 instance-1]" {
		t.Fatalf("unexpected failure report: %+v", failed.Outcomes)
	}
}

func TestReferenceWithoutConfigurationIsNotRun(t *testing.T) {
	t.Setenv("CAMUNDA8_REST_URL", "")
	t.Setenv("CAMUNDA8_ISOLATED", "")
	report, err := RunReferenceFromEnv(context.Background(), DefaultScenarios())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Outcomes) == 0 {
		t.Fatal("expected per-capability NOT RUN outcomes")
	}
	for _, outcome := range report.Outcomes {
		if outcome.Status != StatusNotRun || outcome.Backend != "camunda8" || outcome.Version != "unconfigured" {
			t.Fatalf("no-config status falsely implies a run: %+v", outcome)
		}
	}
}

func TestReferenceRequiresIsolationAcknowledgement(t *testing.T) {
	t.Setenv("CAMUNDA8_REST_URL", "http://127.0.0.1:1")
	t.Setenv("CAMUNDA8_VERSION", "8.x")
	t.Setenv("CAMUNDA8_ISOLATED", "false")
	report, err := RunReferenceFromEnv(context.Background(), DefaultScenarios())
	if err != nil {
		t.Fatal(err)
	}
	for _, outcome := range report.Outcomes {
		if outcome.Status != StatusNotRun || !strings.Contains(outcome.Detail, "ISOLATED") {
			t.Fatalf("expected isolation guard to prevent network run: %+v", outcome)
		}
	}
}

type fakeDriver struct {
	caps map[Capability]bool
}

type fakeControl struct {
	now       time.Time
	advanced  time.Duration
	pumps     int
	failpoint string
}

func (c *fakeControl) Now() time.Time { return c.now }
func (c *fakeControl) AdvanceClock(_ context.Context, duration time.Duration) error {
	c.advanced += duration
	c.now = c.now.Add(duration)
	return nil
}
func (c *fakeControl) Pump(context.Context) error {
	c.pumps++
	return nil
}
func (c *fakeControl) InjectFailpoint(_ context.Context, name string) error {
	c.failpoint = name
	return nil
}

func (d fakeDriver) Backend() string                   { return "fake" }
func (d fakeDriver) Version() string                   { return "test" }
func (d fakeDriver) Capabilities() map[Capability]bool { return d.caps }
func (d fakeDriver) Deploy(context.Context, []File) (Definition, error) {
	return Definition{}, nil
}
func (d fakeDriver) Start(context.Context, string, map[string]any) (Instance, error) {
	return Instance{}, nil
}
func (d fakeDriver) AwaitOpenTasks(context.Context, Instance, string, int) ([]Task, error) {
	return nil, nil
}
func (d fakeDriver) ReadForm(context.Context, Task) (any, error)           { return nil, nil }
func (d fakeDriver) Submit(context.Context, Task, map[string]string) error { return nil }
func (d fakeDriver) AwaitInstance(context.Context, Instance, func(Snapshot) bool) (Snapshot, error) {
	return Snapshot{}, nil
}
func (d fakeDriver) WaitDeadline(context.Context, Task) error { return nil }

func writeJSONTest(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
}
