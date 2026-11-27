package engine

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/ssge/workflow/db/store"
	"github.com/ssge/workflow/internal/application"
	"github.com/ssge/workflow/internal/evaluator"
	"github.com/ssge/workflow/internal/model"
)

func (e *Engine) apply(tx store.Tx, item store.WorkItem) (*store.Outcome, error) {
	switch item.Kind {
	case store.WorkCommand:
		op, err := tx.GetOperation(item.OperationID)
		if err != nil {
			return nil, err
		}
		return e.applyCommand(tx, op)
	case store.WorkStep:
		if err := e.step(tx, item); err != nil {
			return nil, fmt.Errorf("step activation %q: %w", item.ActivationID, err)
		}
		return nil, nil
	case store.WorkTimer:
		if err := e.fireTimer(tx, item); err != nil {
			return nil, fmt.Errorf("fire timer for activation %q: %w", item.ActivationID, err)
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected work kind %q", item.Kind)
	}
}

func (e *Engine) applyCommand(tx store.Tx, op store.Operation) (*store.Outcome, error) {
	switch op.Command.Kind {
	case store.Deploy:
		var payload struct {
			Files []model.Source `json:"files"`
		}
		if err := json.Unmarshal([]byte(op.Command.Payload), &payload); err != nil {
			return nil, err
		}
		return e.publish(tx, payload.Files)
	case store.Start:
		var payload struct {
			ProcessKey string          `json:"processKey"`
			Variables  json.RawMessage `json:"variables"`
		}
		if err := json.Unmarshal([]byte(op.Command.Payload), &payload); err != nil {
			return nil, err
		}
		return e.start(tx, op.ID, payload.ProcessKey, store.Object(payload.Variables))
	case store.CompleteTask:
		var payload struct {
			Fields map[string]any `json:"fields"`
		}
		if err := json.Unmarshal([]byte(op.Command.Payload), &payload); err != nil {
			return nil, err
		}
		var target map[string]string
		if err := json.Unmarshal([]byte(op.Command.Target), &target); err != nil {
			return nil, err
		}
		return e.completeTask(tx, target["taskId"], payload.Fields)
	default:
		return nil, fmt.Errorf("unexpected command kind %q", op.Command.Kind)
	}
}

type diagnostic struct {
	File      string `json:"file"`
	ElementID string `json:"elementId,omitempty"`
	Field     string `json:"field,omitempty"`
	Reason    string `json:"reason"`
}

func (e *Engine) publish(tx store.Tx, files []model.Source) (*store.Outcome, error) {
	plan, problems := (model.Compiler{}).Compile(files)
	if len(problems) != 0 {
		diagnostics := make([]diagnostic, 0, len(problems))
		for _, p := range problems {
			diagnostics = append(diagnostics, diagnostic{p.File(), p.ElementID(), p.Field(), p.Reason()})
		}
		return failure(string(problems[0].Code()), "model compilation failed", diagnostics)
	}
	sources, err := json.Marshal(plan.Sources())
	if err != nil {
		return nil, err
	}
	compiled, err := encodeObject(plan)
	if err != nil {
		return nil, err
	}
	ref, err := tx.Publish(store.Publication{
		ProcessKey: plan.ProcessKey(), ProfileRevision: plan.ProfileRevision(),
		Sources: store.Array(sources), Plan: compiled, CreatedAt: e.clock.Now().UnixMilli(),
	})
	if err != nil {
		return nil, err
	}
	return success(commandResult{ProcessKey: ref.ProcessKey, Version: ref.Version})
}

func loadPlan(tx store.Tx, ref store.DefinitionRef) (*model.Plan, error) {
	def, err := tx.GetDefinition(ref)
	if err != nil {
		return nil, err
	}
	var plan model.Plan
	if err := json.Unmarshal([]byte(def.Plan), &plan); err != nil {
		return nil, fmt.Errorf("restore plan %v: %w", ref, err)
	}
	if plan.ProcessKey() != ref.ProcessKey || plan.ProfileRevision() != def.ProfileRevision {
		return nil, fmt.Errorf("compiled plan identity does not match definition %v", ref)
	}
	return &plan, nil
}

func (e *Engine) start(tx store.Tx, operationID, processKey string, variables store.Object) (*store.Outcome, error) {
	now := e.clock.Now().UnixMilli()
	in, err := tx.CreateInstanceAtHead(store.InstanceStart{
		ID: "instance-" + operationID, ProcessKey: processKey, Variables: variables, CreatedAt: now,
	})
	if errors.Is(err, store.ErrNotFound) {
		return failure("NOT_FOUND", "process has no published definition", nil)
	}
	if err != nil {
		return nil, err
	}
	plan, err := loadPlan(tx, in.Ref)
	if err != nil {
		return nil, err
	}
	if err := enqueue(tx, in.ID, "token-"+in.ID, plan.StartID(), now); err != nil {
		return nil, err
	}
	return success(commandResult{InstanceID: in.ID, ProcessKey: in.Ref.ProcessKey, Version: in.Ref.Version})
}

func (e *Engine) step(tx store.Tx, item store.WorkItem) error {
	in, err := tx.GetInstance(item.InstanceID)
	if err != nil {
		return err
	}
	visit, err := tx.GetActivation(item.ActivationID)
	if err != nil {
		return err
	}
	if visit.InstanceID != in.ID {
		return fmt.Errorf("activation %q does not belong to instance %q", visit.ID, in.ID)
	}
	if in.Status != store.InstanceRunning || visit.State != store.ActivationReady {
		return nil // Obsolete wake-up.
	}
	plan, err := loadPlan(tx, in.Ref)
	if err != nil {
		return err
	}
	node, ok := plan.Node(visit.ElementID)
	if !ok {
		return fmt.Errorf("element %q is absent from pinned plan %v", visit.ElementID, in.Ref)
	}
	now := e.clock.Now().UnixMilli()
	change := store.InstanceChange{
		ID: in.ID, ExpectedRevision: in.Revision, Status: in.Status, Variables: in.Variables,
		Entry: store.JournalEntry{Kind: store.TransitionStep, ElementID: &node.ID, AtMS: now},
	}
	branch, branchErr := tx.GetBranchActivation(visit.ID)
	if errors.Is(branchErr, store.ErrNotFound) {
		branch = store.BranchActivation{}
	} else if branchErr != nil {
		return branchErr
	}
	var next string
	switch node.Kind {
	case model.StartEvent:
		outgoing := node.Outgoing()
		if len(outgoing) != 1 {
			return fmt.Errorf("invalid start step at %q", node.ID)
		}
		vars, err := evaluator.ParseVariables(json.RawMessage(in.Variables))
		if err != nil {
			return err
		}
		for _, mapping := range node.OutputMappings {
			value, err := eval(plan, mapping.Source, vars, node.ID, in.Revision)
			if err != nil {
				return err
			}
			vars[mapping.Target] = value
		}
		change.Variables, err = marshalVariables(vars)
		if err != nil {
			return err
		}
		flow, ok := plan.Flow(outgoing[0])
		if !ok {
			return fmt.Errorf("missing outgoing flow %q at %q", outgoing[0], node.ID)
		}
		next = flow.TargetRef
	case model.EndEvent:
		change.Status, change.EndedAt, change.Entry.Kind = store.InstanceCompleted, &now, store.TransitionCompleted
	case model.UserTask:
		template, ok := plan.Form(node.FormID)
		if !ok {
			return fmt.Errorf("form %q is absent from pinned plan %v", node.FormID, in.Ref)
		}
		vars, err := evaluator.ParseVariables(json.RawMessage(in.Variables))
		if err != nil {
			return err
		}
		view, err := template.Render(vars)
		if err != nil {
			return runtimeFormError(err, in.Revision, node.ID)
		}
		components, err := json.Marshal(view.Components)
		if err != nil {
			return err
		}
		taskID, err := newID("task-")
		if err != nil {
			return err
		}
		if err := tx.InsertUserTask(store.NewUserTask{
			ID: taskID, ActivationID: visit.ID, FormID: node.FormID,
			Components: store.Array(components), CreatedAt: now,
		}); err != nil {
			return err
		}
		if err := tx.SetActivationState(visit.ID, store.ActivationReady, store.ActivationWaiting); err != nil {
			return err
		}
		if boundary, ok := plan.Boundary(node.ID); ok {
			if now > math.MaxInt64-boundary.DurationSeconds*1000 {
				return fmt.Errorf("boundary timer deadline overflows")
			}
			dueAt := now + boundary.DurationSeconds*1000
			timerID, err := newID("timer-")
			if err != nil {
				return err
			}
			if err := tx.InsertBoundaryTimer(store.NewBoundaryTimer{
				ID: timerID, ActivationID: visit.ID, ElementID: boundary.ID, DueAt: dueAt,
			}); err != nil {
				return err
			}
			if err := tx.EnqueueTimerWork("timer-work-"+visit.ID, visit.ID); err != nil {
				return err
			}
		}
		return tx.ChangeInstance(change)
	case model.ScriptTask:
		vars, err := evaluator.ParseVariables(json.RawMessage(in.Variables))
		if err != nil {
			return err
		}
		value, err := eval(plan, node.Expression, vars, node.ID, in.Revision)
		if err != nil {
			return err
		}
		vars[node.ResultVariable] = value
		change.Variables, err = marshalVariables(vars)
		if err != nil {
			return err
		}
		next, err = nextTarget(plan, node)
		if err != nil {
			return err
		}
	case model.BusinessRuleTask:
		vars, err := evaluator.ParseVariables(json.RawMessage(in.Variables))
		if err != nil {
			return err
		}
		decision, ok := plan.Decision(node.DecisionID)
		if !ok {
			return fmt.Errorf("decision %q is absent from pinned plan %v", node.DecisionID, in.Ref)
		}
		value, err := decision.Evaluate(vars)
		if err != nil {
			return runtimeEvaluationError(err, in.Revision, node.ID)
		}
		vars[node.ResultVariable] = value
		change.Variables, err = marshalVariables(vars)
		if err != nil {
			return err
		}
		next, err = nextTarget(plan, node)
		if err != nil {
			return err
		}
	case model.ExclusiveGateway:
		if len(node.Outgoing()) == 1 {
			next, err = nextTarget(plan, node)
			if err != nil {
				return err
			}
			break
		}
		vars, err := evaluator.ParseVariables(json.RawMessage(in.Variables))
		if err != nil {
			return err
		}
		for _, flowID := range node.Outgoing() {
			flow, ok := plan.Flow(flowID)
			if !ok {
				return fmt.Errorf("missing outgoing flow %q at %q", flowID, node.ID)
			}
			if flowID == node.DefaultFlow {
				continue
			}
			result, err := eval(plan, flow.Condition, vars, node.ID, in.Revision)
			if err != nil {
				return err
			}
			condition, ok := result.(bool)
			if !ok {
				return executionFailure{code: "EVALUATION_ERROR", elementID: node.ID, revision: in.Revision, message: "gateway condition did not return a Boolean"}
			}
			if condition {
				next = flow.TargetRef
				break
			}
		}
		if next == "" {
			if node.DefaultFlow == "" {
				return executionFailure{code: "NO_ROUTE", elementID: node.ID, revision: in.Revision, message: "exclusive gateway has no matching route"}
			}
			flow, ok := plan.Flow(node.DefaultFlow)
			if !ok {
				return fmt.Errorf("missing default flow %q at %q", node.DefaultFlow, node.ID)
			}
			next = flow.TargetRef
		}
	case model.ParallelGateway:
		if len(node.Incoming()) == 1 {
			join, ok := plan.ParallelPeer(node.ID)
			if !ok {
				return fmt.Errorf("parallel split %q has no matching join", node.ID)
			}
			forkID, err := newID("fork-")
			if err != nil {
				return err
			}
			if err := tx.CreateFork(store.NewFork{
				ID: forkID, InstanceID: in.ID, SplitActivationID: visit.ID, JoinElementID: join,
			}); err != nil {
				return err
			}
			if err := tx.SetActivationState(visit.ID, store.ActivationReady, store.ActivationConsumed); err != nil {
				return err
			}
			for _, flowID := range node.Outgoing() {
				flow, ok := plan.Flow(flowID)
				if !ok {
					return fmt.Errorf("missing split flow %q", flowID)
				}
				tokenID, err := newID("token-")
				if err != nil {
					return err
				}
				activationID, err := enqueueToken(tx, in.ID, tokenID, flow.TargetRef, now, nil)
				if err != nil {
					return err
				}
				if err := tx.InsertBranchActivation(store.BranchActivation{
					ActivationID: activationID, InstanceID: in.ID, ForkID: forkID, BranchFlowID: flowID,
				}); err != nil {
					return err
				}
			}
			return tx.ChangeInstance(change)
		}
		if branch.ActivationID == "" {
			return fmt.Errorf("join activation %q is not associated with a fork", visit.ID)
		}
		fork, err := tx.GetFork(branch.ForkID)
		if err != nil {
			return fmt.Errorf("load fork %q: %w", branch.ForkID, err)
		}
		if fork.State != store.ForkOpen || fork.JoinElementID != node.ID {
			return nil
		}
		splitActivation, err := tx.GetActivation(fork.SplitActivationID)
		if err != nil {
			return fmt.Errorf("load fork split activation %q: %w", fork.SplitActivationID, err)
		}
		split, ok := plan.Node(splitActivation.ElementID)
		if !ok {
			return fmt.Errorf("split %q is absent from pinned plan", splitActivation.ElementID)
		}
		incoming, ok := branchJoinFlow(plan, split, branch.BranchFlowID, node.ID)
		if !ok {
			return fmt.Errorf("cannot identify join input for branch %q", branch.BranchFlowID)
		}
		if err := tx.InsertJoinArrival(store.JoinArrival{
			ForkID: fork.ID, IncomingFlowID: incoming, BranchFlowID: branch.BranchFlowID, ActivationID: visit.ID,
		}); err != nil {
			return fmt.Errorf("record join arrival for branch %q on flow %q: %w", branch.BranchFlowID, incoming, err)
		}
		if err := tx.SetActivationState(visit.ID, store.ActivationReady, store.ActivationWaiting); err != nil {
			return fmt.Errorf("park join activation %q: %w", visit.ID, err)
		}
		arrivals, err := tx.ListJoinArrivals(fork.ID)
		if err != nil {
			return fmt.Errorf("list arrivals for fork %q: %w", fork.ID, err)
		}
		if len(arrivals) < len(node.Incoming()) {
			if err := tx.ChangeInstance(change); err != nil {
				return fmt.Errorf("commit parked join arrival at revision %d: %w", in.Revision, err)
			}
			return nil
		}
		if len(arrivals) != len(node.Incoming()) {
			return fmt.Errorf("fork %q has too many arrivals", fork.ID)
		}
		if err := tx.CloseFork(fork.ID); err != nil {
			return fmt.Errorf("close fork %q: %w", fork.ID, err)
		}
		for _, arrival := range arrivals {
			if err := tx.SetActivationState(arrival.ActivationID, store.ActivationWaiting, store.ActivationConsumed); err != nil {
				return fmt.Errorf("consume joined activation %q: %w", arrival.ActivationID, err)
			}
		}
		branch = store.BranchActivation{}
		next, err = nextTarget(plan, node)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported step %q at %q", node.Kind, node.ID)
	}
	if err := tx.ChangeInstance(change); err != nil {
		return fmt.Errorf("commit step for %q at revision %d: %w", node.ID, in.Revision, err)
	}
	if node.Kind != model.ParallelGateway {
		if err := tx.SetActivationState(visit.ID, store.ActivationReady, store.ActivationConsumed); err != nil {
			return err
		}
	}
	if next != "" {
		_, err := enqueueToken(tx, in.ID, visit.TokenID, next, now, branchPointer(branch))
		return err
	}
	return nil
}

func (e *Engine) completeTask(tx store.Tx, taskID string, fields map[string]any) (*store.Outcome, error) {
	task, err := tx.GetUserTask(taskID)
	if errors.Is(err, store.ErrNotFound) {
		return failure("NOT_FOUND", "task not found", nil)
	}
	if err != nil {
		return nil, err
	}
	if task.Status != store.TaskOpen {
		return failure("TASK_NOT_OPEN", "task is no longer open", nil)
	}
	activation, err := tx.GetActivation(task.ActivationID)
	if err != nil {
		return nil, err
	}
	instance, err := tx.GetInstance(activation.InstanceID)
	if err != nil {
		return nil, err
	}
	if instance.Status == store.InstanceIncident {
		return failure("INSTANCE_IN_INCIDENT", "instance is frozen by an incident", nil)
	}
	if instance.Status != store.InstanceRunning || activation.State != store.ActivationWaiting {
		return failure("TASK_NOT_OPEN", "task is no longer open", nil)
	}
	plan, err := loadPlan(tx, instance.Ref)
	if err != nil {
		return nil, err
	}
	node, ok := plan.Node(activation.ElementID)
	if !ok || node.Kind != model.UserTask {
		return nil, fmt.Errorf("task activation %q is not a user task", activation.ID)
	}
	template, ok := plan.Form(task.FormID)
	if !ok {
		return nil, fmt.Errorf("form %q is absent from pinned plan %v", task.FormID, instance.Ref)
	}
	writes, fieldErrors := template.ValidateSubmission(fields)
	if len(fieldErrors) != 0 {
		details := make([]map[string]string, 0, len(fieldErrors))
		for _, field := range fieldErrors {
			details = append(details, map[string]string{"key": field.Key, "reason": field.Reason})
		}
		out, err := encodeObject(application.Failure{
			Code: "INVALID_FIELDS", Message: "submission contains invalid fields",
			Details: map[string]any{"fields": details},
		})
		if err != nil {
			return nil, err
		}
		return &store.Outcome{Error: &out}, nil
	}
	vars, err := evaluator.ParseVariables(json.RawMessage(instance.Variables))
	if err != nil {
		return nil, err
	}
	for key, value := range writes {
		vars[key] = value
	}
	updated, err := marshalVariables(vars)
	if err != nil {
		return nil, err
	}
	now := e.clock.Now().UnixMilli()
	if err := tx.CloseUserTask(task.ID, store.TaskClosure{Status: store.TaskCompleted, ClosedAt: now}); err != nil {
		return nil, err
	}
	if _, err := tx.GetBoundaryTimer(activation.ID); err == nil {
		if err := tx.TransitionBoundaryTimer(activation.ID, store.TimerCancelled); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if err := tx.SetActivationState(activation.ID, store.ActivationWaiting, store.ActivationConsumed); err != nil {
		return nil, err
	}
	branch, err := tx.GetBranchActivation(activation.ID)
	if errors.Is(err, store.ErrNotFound) {
		branch = store.BranchActivation{}
	} else if err != nil {
		return nil, err
	}
	next, err := nextTarget(plan, node)
	if err != nil {
		return nil, err
	}
	element := node.ID
	revision := instance.Revision + 1
	if err := tx.ChangeInstance(store.InstanceChange{
		ID: instance.ID, ExpectedRevision: instance.Revision, Status: store.InstanceRunning, Variables: updated,
		Entry: store.JournalEntry{Kind: store.TransitionStep, ElementID: &element, AtMS: now},
	}); err != nil {
		return nil, err
	}
	if _, err := enqueueToken(tx, instance.ID, activation.TokenID, next, now, branchPointer(branch)); err != nil {
		return nil, err
	}
	out, err := encodeObject(struct {
		TaskID     string `json:"taskId"`
		InstanceID string `json:"instanceId"`
		Revision   int64  `json:"revision"`
	}{task.ID, instance.ID, revision})
	if err != nil {
		return nil, err
	}
	return &store.Outcome{Result: &out}, nil
}

func (e *Engine) fireTimer(tx store.Tx, item store.WorkItem) error {
	timer, err := tx.GetBoundaryTimer(item.ActivationID)
	if err != nil {
		return err
	}
	if timer.State != store.TimerArmed {
		return nil
	}
	task, err := tx.GetUserTaskByActivation(item.ActivationID)
	if err != nil {
		return err
	}
	activation, err := tx.GetActivation(item.ActivationID)
	if err != nil {
		return err
	}
	instance, err := tx.GetInstance(activation.InstanceID)
	if err != nil {
		return err
	}
	if instance.Status == store.InstanceIncident {
		return store.ErrConflict
	}
	if instance.Status != store.InstanceRunning || task.Status != store.TaskOpen || activation.State != store.ActivationWaiting {
		return nil
	}
	plan, err := loadPlan(tx, instance.Ref)
	if err != nil {
		return err
	}
	boundary, ok := plan.Node(timer.ElementID)
	if !ok || boundary.Kind != model.BoundaryEvent {
		return fmt.Errorf("boundary %q is absent from pinned plan", timer.ElementID)
	}
	now := e.clock.Now().UnixMilli()
	reason := store.TaskCancelledByTimer
	if err := tx.CloseUserTask(task.ID, store.TaskClosure{Status: store.TaskCancelled, Reason: &reason, ClosedAt: now}); err != nil {
		return err
	}
	if err := tx.TransitionBoundaryTimer(activation.ID, store.TimerFired); err != nil {
		return err
	}
	if err := tx.SetActivationState(activation.ID, store.ActivationWaiting, store.ActivationConsumed); err != nil {
		return err
	}
	branch, err := tx.GetBranchActivation(activation.ID)
	if errors.Is(err, store.ErrNotFound) {
		branch = store.BranchActivation{}
	} else if err != nil {
		return err
	}
	next, err := nextTarget(plan, boundary)
	if err != nil {
		return err
	}
	element := boundary.ID
	if err := tx.ChangeInstance(store.InstanceChange{
		ID: instance.ID, ExpectedRevision: instance.Revision, Status: store.InstanceRunning, Variables: instance.Variables,
		Entry: store.JournalEntry{Kind: store.TransitionStep, ElementID: &element, AtMS: now},
	}); err != nil {
		return err
	}
	_, err = enqueueToken(tx, instance.ID, activation.TokenID, next, now, branchPointer(branch))
	return err
}

type executionFailure struct {
	code, elementID, message string
	revision                 int64
}

func (e executionFailure) Error() string { return e.code + ": " + e.message }

func runtimeEvaluationError(err error, revision int64, elementID string) error {
	return executionFailure{code: "EVALUATION_ERROR", elementID: elementID, revision: revision, message: err.Error()}
}

func runtimeFormError(err error, revision int64, elementID string) error {
	return executionFailure{code: "EVALUATION_ERROR", elementID: elementID, revision: revision, message: err.Error()}
}

func eval(plan *model.Plan, source string, vars map[string]any, elementID string, revision int64) (any, error) {
	expression, ok := plan.Expression(source)
	if !ok {
		return nil, fmt.Errorf("expression %q is absent from pinned plan", source)
	}
	value, err := expression(vars)
	if err != nil {
		return nil, executionFailure{code: "EVALUATION_ERROR", elementID: elementID, revision: revision, message: err.Error()}
	}
	return value, nil
}

func marshalVariables(vars map[string]any) (store.Object, error) {
	raw, err := evaluator.MarshalVariables(vars)
	return store.Object(raw), err
}

func nextTarget(plan *model.Plan, node model.Node) (string, error) {
	outgoing := node.Outgoing()
	if len(outgoing) != 1 {
		return "", fmt.Errorf("element %q has %d outgoing flows", node.ID, len(outgoing))
	}
	flow, ok := plan.Flow(outgoing[0])
	if !ok {
		return "", fmt.Errorf("missing outgoing flow %q at %q", outgoing[0], node.ID)
	}
	return flow.TargetRef, nil
}

func branchJoinFlow(plan *model.Plan, split model.Node, branchFlow, joinID string) (string, bool) {
	first, ok := plan.Flow(branchFlow)
	if !ok {
		return "", false
	}
	queue := []string{first.TargetRef}
	seen := map[string]bool{}
	var incoming string
	for len(queue) != 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		node, ok := plan.Node(id)
		if !ok {
			return "", false
		}
		for _, flowID := range node.Outgoing() {
			flow, ok := plan.Flow(flowID)
			if !ok {
				return "", false
			}
			if flow.TargetRef == joinID {
				if incoming != "" && incoming != flow.ID {
					return "", false
				}
				incoming = flow.ID
				continue
			}
			target, ok := plan.Node(flow.TargetRef)
			if !ok || target.ID == split.ID || target.Kind == model.ParallelGateway {
				continue
			}
			queue = append(queue, target.ID)
		}
	}
	return incoming, incoming != ""
}

func branchPointer(branch store.BranchActivation) *store.BranchActivation {
	if branch.ActivationID == "" {
		return nil
	}
	return &branch
}

func enqueue(tx store.Tx, instance, token, element string, now int64) error {
	_, err := enqueueToken(tx, instance, token, element, now, nil)
	return err
}

func enqueueToken(tx store.Tx, instance, token, element string, now int64, branch *store.BranchActivation) (string, error) {
	id, err := newID("activation-")
	if err != nil {
		return "", err
	}
	if err := tx.InsertActivation(store.Activation{
		ID: id, InstanceID: instance, TokenID: token, ElementID: element, State: store.ActivationReady,
	}); err != nil {
		return "", err
	}
	if err := tx.EnqueueStep(store.StepWork{ID: "step-" + id, InstanceID: instance, ActivationID: id, ReadyAt: now}); err != nil {
		return "", err
	}
	if branch != nil {
		if err := tx.InsertBranchActivation(store.BranchActivation{
			ActivationID: id, InstanceID: instance, ForkID: branch.ForkID, BranchFlowID: branch.BranchFlowID,
		}); err != nil {
			return "", err
		}
	}
	return id, nil
}

func newID(prefix string) (string, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%x", prefix, entropy), nil
}

type commandResult struct {
	InstanceID string `json:"instanceId,omitempty"`
	ProcessKey string `json:"processKey"`
	Version    int64  `json:"version"`
}

func success(result commandResult) (*store.Outcome, error) {
	raw, err := encodeObject(result)
	if err != nil {
		return nil, err
	}
	return &store.Outcome{Result: &raw}, nil
}

func failure(code, message string, diagnostics []diagnostic) (*store.Outcome, error) {
	value := application.Failure{Code: code, Message: message}
	if len(diagnostics) != 0 {
		value.Details = map[string]any{"diagnostics": diagnostics}
	}
	raw, err := encodeObject(value)
	if err != nil {
		return nil, err
	}
	return &store.Outcome{Error: &raw}, nil
}
