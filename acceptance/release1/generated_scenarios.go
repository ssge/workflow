package release1

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	p5LeftElement  = "Activity_Left"
	p5RightElement = "Activity_Right"
	p6TaskElement  = "Activity_Work"
)

func P5ParallelJoinScenario() Scenario {
	return generatedScenario("P5-parallel-join", []Capability{
		CapabilityDeployment, CapabilityStart, CapabilityTaskQuery,
		CapabilityTaskSubmit, CapabilityInstanceQuery,
	}, runP5)
}

func P6PinnedVersionScenario() Scenario {
	return generatedScenario("P6-pinned-version-path", []Capability{
		CapabilityDeployment, CapabilityStart, CapabilityTaskQuery,
		CapabilityTaskSubmit, CapabilityInstanceQuery,
	}, runP6)
}

func runP5(ctx context.Context, e *fixtureExecution) error {
	processKey, err := UniqueProcessKey()
	if err != nil {
		return err
	}
	files, err := parallelJoinFiles(processKey)
	if err != nil {
		return err
	}
	if err := e.deploy(ctx, files); err != nil {
		return err
	}

	for _, reverse := range []bool{false, true} {
		instance, err := e.start(ctx, map[string]any{"joined": 0})
		if err != nil {
			return err
		}
		tasks, err := e.tasks(ctx, instance, "", 2)
		if err != nil {
			return err
		}
		left, right, err := pickParallelTasks(tasks)
		if err != nil {
			return err
		}
		first, second := left, right
		firstField, secondField := "left", "right"
		firstValue, secondValue := "L", "R"
		if reverse {
			first, second = right, left
			firstField, secondField = "right", "left"
			firstValue, secondValue = "R", "L"
		}
		if err := e.submit(ctx, first, map[string]string{firstField: firstValue}); err != nil {
			return err
		}

		siblings, err := e.tasks(ctx, instance, "", 1)
		if err != nil {
			return err
		}
		if len(siblings) != 1 || siblings[0].ID != second.ID {
			return fmt.Errorf("expected branch sibling %q to remain open, got %+v", second.ID, siblings)
		}
		intermediate, err := e.await(ctx, instance, func(snapshot Snapshot) bool {
			return snapshot.State == "RUNNING" && numberEquals(snapshot.Variables["joined"], 0)
		})
		if err != nil {
			return err
		}
		if intermediate.Variables[firstField] != firstValue {
			return fmt.Errorf("first branch write %s=%q was not retained: %v", firstField, firstValue, intermediate.Variables)
		}
		if err := e.submit(ctx, second, map[string]string{secondField: secondValue}); err != nil {
			return err
		}
		final, err := e.await(ctx, instance, func(snapshot Snapshot) bool { return snapshot.State == "COMPLETED" })
		if err != nil {
			return err
		}
		if final.Variables["left"] != "L" || final.Variables["right"] != "R" {
			return fmt.Errorf("parallel branch writes were not both retained: %v", final.Variables)
		}
		if !numberEquals(final.Variables["joined"], 1) {
			return fmt.Errorf("parallel join successor ran an unexpected number of times: joined=%v", final.Variables["joined"])
		}
		if err := e.noOpenTasksFor(ctx, instance, ""); err != nil {
			return err
		}
	}
	return nil
}

func pickParallelTasks(tasks []Task) (Task, Task, error) {
	var left, right Task
	for _, task := range tasks {
		switch task.ElementID {
		case p5LeftElement:
			left = task
		case p5RightElement:
			right = task
		default:
			return Task{}, Task{}, fmt.Errorf("unexpected parallel task element %q", task.ElementID)
		}
	}
	if left.ID == "" || right.ID == "" || left.ID == right.ID {
		return Task{}, Task{}, fmt.Errorf("expected distinct left and right task activations, got %+v", tasks)
	}
	return left, right, nil
}

func runP6(ctx context.Context, e *fixtureExecution) error {
	processKey, err := UniqueProcessKey()
	if err != nil {
		return err
	}
	v1Files, v2Files, err := pinnedVersionFiles(processKey)
	if err != nil {
		return err
	}
	v1, err := e.driver.Deploy(ctx, v1Files)
	if err != nil {
		return fmt.Errorf("deploy v1 model bundle: %w", err)
	}
	e.definition = v1
	e.handles = append(e.handles, v1.Handle)

	oldInstance, err := e.start(ctx, map[string]any{})
	if err != nil {
		return err
	}
	oldTask, err := e.taskForElement(ctx, oldInstance, p6TaskElement)
	if err != nil {
		return err
	}

	v2, err := e.driver.Deploy(ctx, v2Files)
	if err != nil {
		return fmt.Errorf("deploy v2 model bundle: %w", err)
	}
	e.handles = append(e.handles, v2.Handle)
	if v1.Version == "" || v2.Version == "" || v1.Version == v2.Version {
		return fmt.Errorf("deployments did not produce distinct version handles: v1=%q v2=%q", v1.Version, v2.Version)
	}

	e.definition = v2
	newInstance, err := e.start(ctx, map[string]any{})
	if err != nil {
		return err
	}
	newTask, err := e.taskForElement(ctx, newInstance, p6TaskElement)
	if err != nil {
		return err
	}
	if oldTask.ID == newTask.ID {
		return fmt.Errorf("separate process versions reused task handle %q", oldTask.ID)
	}

	if err := e.submit(ctx, oldTask, map[string]string{"proceed": "continue"}); err != nil {
		return err
	}
	oldFinal, err := e.await(ctx, oldInstance, func(snapshot Snapshot) bool { return snapshot.State == "COMPLETED" })
	if err != nil {
		return err
	}
	if oldFinal.Definition.Version != v1.Version || oldInstance.Version != v1.Version {
		return fmt.Errorf("old instance lost its v1 pin: deploy=%q start=%q query=%q",
			v1.Version, oldInstance.Version, oldFinal.Definition.Version)
	}
	if oldFinal.Variables["marker"] != "old" {
		return fmt.Errorf("old instance used the wrong process path: marker=%v", oldFinal.Variables["marker"])
	}

	if err := e.submit(ctx, newTask, map[string]string{"proceed": "continue"}); err != nil {
		return err
	}
	newFinal, err := e.await(ctx, newInstance, func(snapshot Snapshot) bool { return snapshot.State == "COMPLETED" })
	if err != nil {
		return err
	}
	if newFinal.Definition.Version != v2.Version || newInstance.Version != v2.Version {
		return fmt.Errorf("new instance did not use v2: deploy=%q start=%q query=%q",
			v2.Version, newInstance.Version, newFinal.Definition.Version)
	}
	if newFinal.Variables["marker"] != "new" {
		return fmt.Errorf("new instance used the wrong process path: marker=%v", newFinal.Variables["marker"])
	}
	if err := e.noOpenTasksFor(ctx, oldInstance, p6TaskElement); err != nil {
		return err
	}
	return e.noOpenTasksFor(ctx, newInstance, p6TaskElement)
}

func (e *fixtureExecution) taskForElement(ctx context.Context, instance Instance, elementID string) (Task, error) {
	tasks, err := e.tasks(ctx, instance, elementID, 1)
	if err != nil {
		return Task{}, err
	}
	return tasks[0], nil
}

func parallelJoinFiles(processKey string) ([]File, error) {
	leftForm, err := textFieldForm("Form_Left", "left", "Left")
	if err != nil {
		return nil, err
	}
	rightForm, err := textFieldForm("Form_Right", "right", "Right")
	if err != nil {
		return nil, err
	}
	model := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<bpmn:definitions xmlns:bpmn="http://www.omg.org/spec/BPMN/20100524/MODEL" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:zeebe="http://camunda.org/schema/zeebe/1.0" id="Definitions_P5" targetNamespace="http://bpmn.io/schema/bpmn">
  <bpmn:process id="%s" isExecutable="true">
    <bpmn:startEvent id="Start"><bpmn:outgoing>Flow_Start_Fork</bpmn:outgoing></bpmn:startEvent>
    <bpmn:parallelGateway id="Fork"><bpmn:incoming>Flow_Start_Fork</bpmn:incoming><bpmn:outgoing>Flow_Fork_Left</bpmn:outgoing><bpmn:outgoing>Flow_Fork_Right</bpmn:outgoing></bpmn:parallelGateway>
    <bpmn:userTask id="%s"><bpmn:extensionElements><zeebe:userTask/><zeebe:formDefinition formId="Form_Left"/></bpmn:extensionElements><bpmn:incoming>Flow_Fork_Left</bpmn:incoming><bpmn:outgoing>Flow_Left_Join</bpmn:outgoing></bpmn:userTask>
    <bpmn:userTask id="%s"><bpmn:extensionElements><zeebe:userTask/><zeebe:formDefinition formId="Form_Right"/></bpmn:extensionElements><bpmn:incoming>Flow_Fork_Right</bpmn:incoming><bpmn:outgoing>Flow_Right_Join</bpmn:outgoing></bpmn:userTask>
    <bpmn:parallelGateway id="Join"><bpmn:incoming>Flow_Left_Join</bpmn:incoming><bpmn:incoming>Flow_Right_Join</bpmn:incoming><bpmn:outgoing>Flow_Join_Increment</bpmn:outgoing></bpmn:parallelGateway>
    <bpmn:scriptTask id="IncrementJoined"><bpmn:extensionElements><zeebe:script expression="= joined + 1" resultVariable="joined"/></bpmn:extensionElements><bpmn:incoming>Flow_Join_Increment</bpmn:incoming><bpmn:outgoing>Flow_Increment_End</bpmn:outgoing></bpmn:scriptTask>
    <bpmn:endEvent id="End"><bpmn:incoming>Flow_Increment_End</bpmn:incoming></bpmn:endEvent>
    <bpmn:sequenceFlow id="Flow_Start_Fork" sourceRef="Start" targetRef="Fork"/>
    <bpmn:sequenceFlow id="Flow_Fork_Left" sourceRef="Fork" targetRef="%s"/>
    <bpmn:sequenceFlow id="Flow_Fork_Right" sourceRef="Fork" targetRef="%s"/>
    <bpmn:sequenceFlow id="Flow_Left_Join" sourceRef="%s" targetRef="Join"/>
    <bpmn:sequenceFlow id="Flow_Right_Join" sourceRef="%s" targetRef="Join"/>
    <bpmn:sequenceFlow id="Flow_Join_Increment" sourceRef="Join" targetRef="IncrementJoined"/>
    <bpmn:sequenceFlow id="Flow_Increment_End" sourceRef="IncrementJoined" targetRef="End"/>
  </bpmn:process>
</bpmn:definitions>`, processKey, p5LeftElement, p5RightElement, p5LeftElement, p5RightElement, p5LeftElement, p5RightElement)
	return []File{
		{Name: "parallel-join.bpmn", Content: model},
		{Name: "form-left.form", Content: leftForm},
		{Name: "form-right.form", Content: rightForm},
	}, nil
}

func pinnedVersionFiles(processKey string) ([]File, []File, error) {
	form, err := textFieldForm("Form_Work", "proceed", "Continue")
	if err != nil {
		return nil, nil, err
	}
	build := func(marker string) []File {
		model := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<bpmn:definitions xmlns:bpmn="http://www.omg.org/spec/BPMN/20100524/MODEL" xmlns:zeebe="http://camunda.org/schema/zeebe/1.0" id="Definitions_P6" targetNamespace="http://bpmn.io/schema/bpmn">
  <bpmn:process id="%s" isExecutable="true">
    <bpmn:startEvent id="Start"><bpmn:outgoing>Flow_Start_Task</bpmn:outgoing></bpmn:startEvent>
    <bpmn:userTask id="%s"><bpmn:extensionElements><zeebe:userTask/><zeebe:formDefinition formId="Form_Work"/></bpmn:extensionElements><bpmn:incoming>Flow_Start_Task</bpmn:incoming><bpmn:outgoing>Flow_Task_Marker</bpmn:outgoing></bpmn:userTask>
    <bpmn:scriptTask id="SetMarker"><bpmn:extensionElements><zeebe:script expression="= &quot;%s&quot;" resultVariable="marker"/></bpmn:extensionElements><bpmn:incoming>Flow_Task_Marker</bpmn:incoming><bpmn:outgoing>Flow_Marker_End</bpmn:outgoing></bpmn:scriptTask>
    <bpmn:endEvent id="End"><bpmn:incoming>Flow_Marker_End</bpmn:incoming></bpmn:endEvent>
    <bpmn:sequenceFlow id="Flow_Start_Task" sourceRef="Start" targetRef="%s"/>
    <bpmn:sequenceFlow id="Flow_Task_Marker" sourceRef="%s" targetRef="SetMarker"/>
    <bpmn:sequenceFlow id="Flow_Marker_End" sourceRef="SetMarker" targetRef="End"/>
  </bpmn:process>
</bpmn:definitions>`, processKey, p6TaskElement, marker, p6TaskElement, p6TaskElement)
		return []File{
			{Name: "pinned-path.bpmn", Content: model},
			{Name: "form-work.form", Content: form},
		}
	}
	return build("old"), build("new"), nil
}

func textFieldForm(formID, key, label string) (string, error) {
	form := map[string]any{
		"components": []map[string]any{{
			"label": label, "type": "textfield",
			"layout": map[string]any{"row": "Row_Input", "columns": nil},
			"id":     "Field_" + key, "key": key,
		}},
		"type": "default", "id": formID,
		"executionPlatform": "Camunda Cloud", "executionPlatformVersion": "8.9.0",
		"exporter":      map[string]string{"name": "Camunda Modeler", "version": "5.49.0"},
		"schemaVersion": 19,
	}
	encoded, err := json.Marshal(form)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func generatedProcessID(files []File) (string, bool) {
	for _, file := range files {
		if !strings.HasSuffix(file.Name, ".bpmn") {
			continue
		}
		const marker = `<bpmn:process id="`
		start := strings.Index(file.Content, marker)
		if start < 0 {
			continue
		}
		start += len(marker)
		end := strings.IndexByte(file.Content[start:], '"')
		if end < 0 {
			continue
		}
		return file.Content[start : start+end], true
	}
	return "", false
}
