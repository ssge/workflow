// Package model compiles logical source bundles into immutable, versioned plans.
// It performs no I/O. Plan restoration uses only the pinned graph and resources.
package model

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/ssge/workflow/form"
	"github.com/ssge/workflow/internal/dmn"
)

const profileRevision = "r1-v1"

// Source holds a logical filename and the original, unmodified text.
type Source struct{ data sourceData }

type sourceData struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

func NewSource(name, content string) Source   { return Source{sourceData{name, content}} }
func (s Source) Name() string                 { return s.data.Name }
func (s Source) Content() string              { return s.data.Content }
func (s Source) MarshalJSON() ([]byte, error) { return json.Marshal(s.data) }
func (s *Source) UnmarshalJSON(b []byte) error {
	var data sourceData
	if err := json.Unmarshal(b, &data); err != nil {
		return err
	}
	s.data = data
	return nil
}

type Code string

const (
	InvalidModel       Code = "INVALID_MODEL"
	UnsupportedFeature Code = "UNSUPPORTED_FEATURE"
)

// Diagnostic locates a compile failure. ElementID is empty for file-level errors.
type Diagnostic struct{ data diagnosticData }

type diagnosticData struct {
	Code      Code   `json:"code"`
	File      string `json:"file"`
	ElementID string `json:"elementId,omitempty"`
	Field     string `json:"field,omitempty"`
	Reason    string `json:"reason"`
}

func problem(code Code, file, id, field, reason string) *Diagnostic {
	return &Diagnostic{diagnosticData{code, file, id, field, reason}}
}

func (d Diagnostic) Code() Code        { return d.data.Code }
func (d Diagnostic) File() string      { return d.data.File }
func (d Diagnostic) ElementID() string { return d.data.ElementID }
func (d Diagnostic) Field() string     { return d.data.Field }
func (d Diagnostic) Reason() string    { return d.data.Reason }
func (d Diagnostic) Error() string {
	return fmt.Sprintf("%s: %s (%s, %s): %s", d.Code(), d.File(), d.ElementID(), d.Field(), d.Reason())
}
func (d Diagnostic) MarshalJSON() ([]byte, error) { return json.Marshal(d.data) }
func (d *Diagnostic) UnmarshalJSON(b []byte) error {
	var data diagnosticData
	if err := json.Unmarshal(b, &data); err != nil {
		return err
	}
	d.data = data
	return nil
}

type NodeKind string

const (
	StartEvent       NodeKind = "startEvent"
	EndEvent         NodeKind = "endEvent"
	UserTask         NodeKind = "userTask"
	ScriptTask       NodeKind = "scriptTask"
	BusinessRuleTask NodeKind = "businessRuleTask"
	ExclusiveGateway NodeKind = "exclusiveGateway"
	ParallelGateway  NodeKind = "parallelGateway"
	BoundaryEvent    NodeKind = "boundaryEvent"
)

type OutputMapping struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

// Node is a flow-node snapshot. Adjacency contains flow IDs in sequenceFlow
// document order, not the order of a node's optional BPMN declarations.
type Node struct {
	ID               string          `json:"id"`
	Kind             NodeKind        `json:"kind"`
	Name             string          `json:"name,omitempty"`
	OutputMappings   []OutputMapping `json:"outputMappings,omitempty"`
	Expression       string          `json:"expression,omitempty"`
	ResultVariable   string          `json:"resultVariable,omitempty"`
	DecisionID       string          `json:"decisionId,omitempty"`
	FormID           string          `json:"formId,omitempty"`
	DefaultFlow      string          `json:"defaultFlow,omitempty"`
	GatewayDirection string          `json:"gatewayDirection,omitempty"`
	AttachedToRef    string          `json:"attachedToRef,omitempty"`
	DurationSeconds  int64           `json:"durationSeconds,omitempty"`

	incoming, outgoing []string
}

func (n Node) Incoming() []string { return slices.Clone(n.incoming) }
func (n Node) Outgoing() []string { return slices.Clone(n.outgoing) }

type SequenceFlow struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	SourceRef string `json:"sourceRef"`
	TargetRef string `json:"targetRef"`
	Condition string `json:"condition,omitempty"`
}

// Only the canonical graph is persisted. Adjacency, indexes, and the start
// pointer are derived facts, rebuilt and checked by the same linker on load.
type planData struct {
	ProfileRevision string         `json:"profileRevision"`
	ProcessKey      string         `json:"processKey"`
	ProcessName     string         `json:"processName,omitempty"`
	Sources         []Source       `json:"sources"`
	Nodes           []Node         `json:"nodes"`
	Flows           []SequenceFlow `json:"flows"`
}

// Plan exposes only value snapshots. Unmarshaling replaces the destination;
// it never modifies a graph shared with an existing Plan value.
type Plan struct {
	data        planData
	nodes       map[string]int
	flows       map[string]int
	start       string
	boundaries  map[string]string
	peers       map[string]string
	expressions map[string]Expression
	forms       map[string]*form.Template
	decisions   map[string]*dmn.Decision
}

func (p Plan) ProfileRevision() string { return p.data.ProfileRevision }
func (p Plan) ProcessKey() string      { return p.data.ProcessKey }
func (p Plan) ProcessName() string     { return p.data.ProcessName }
func (p Plan) StartID() string         { return p.start }
func (p Plan) Sources() []Source       { return slices.Clone(p.data.Sources) }
func (p Plan) Nodes() []Node {
	nodes := slices.Clone(p.data.Nodes)
	for i := range nodes {
		nodes[i].OutputMappings = slices.Clone(nodes[i].OutputMappings)
	}
	return nodes
}
func (p Plan) Flows() []SequenceFlow { return slices.Clone(p.data.Flows) }
func (p Plan) Node(id string) (Node, bool) {
	i, ok := p.nodes[id]
	if !ok {
		return Node{}, false
	}
	n := p.data.Nodes[i]
	n.OutputMappings = slices.Clone(n.OutputMappings)
	return n, true
}
func (p Plan) Flow(id string) (SequenceFlow, bool) {
	i, ok := p.flows[id]
	if !ok {
		return SequenceFlow{}, false
	}
	return p.data.Flows[i], true
}

func (p Plan) MarshalJSON() ([]byte, error) { return json.Marshal(p.data) }
func (p *Plan) UnmarshalJSON(b []byte) error {
	next, err := (Compiler{}).Restore(b)
	if err != nil {
		return err
	}
	*p = *next
	return nil
}

// Restore reads a catalog-held plan (not a source-import entry point). It
// rebuilds indexes and programs from the serialized graph and pinned resource
// sources using this compiler, never the current deployment head.
func (c Compiler) Restore(b []byte) (*Plan, error) {
	var data planData
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, err
	}
	if data.ProfileRevision != profileRevision && data.ProfileRevision != "r1-a4-v1" {
		return nil, fmt.Errorf("unsupported plan profile revision %q", data.ProfileRevision)
	}
	if data.ProfileRevision == "r1-a4-v1" {
		for _, n := range data.Nodes {
			if (n.Kind != StartEvent && n.Kind != EndEvent) || len(n.OutputMappings) != 0 {
				return nil, fmt.Errorf("%s: executable node outside the saved A4 profile", n.ID)
			}
		}
		for _, s := range data.Sources {
			if s.Name() != ".process-application" && !strings.HasSuffix(s.Name(), ".bpmn") {
				return nil, fmt.Errorf("%s: resource outside the saved A4 profile", s.Name())
			}
		}
	}
	src, d := bpmnSource(data.Sources)
	if d != nil {
		return nil, d
	}
	next, d := c.finish(src.Name(), data, nil)
	if d != nil {
		return nil, d
	}
	return next, nil
}

// Expression returns the compiled program for a marked BPMN expression in
// this plan (mapping Source, script Expression, or flow Condition).
func (p Plan) Expression(source string) (Expression, bool) {
	e, ok := p.expressions[source]
	return e, ok
}
func (p Plan) Boundary(hostID string) (Node, bool) { return p.Node(p.boundaries[hostID]) }

// ParallelPeer returns the matching join for a split, or split for a join.
func (p Plan) ParallelPeer(id string) (string, bool) { peer, ok := p.peers[id]; return peer, ok }
func (p Plan) Decision(id string) (*dmn.Decision, bool) {
	d, ok := p.decisions[id]
	if !ok {
		return nil, false
	}
	snapshot := *d
	return &snapshot, true
}
func (p Plan) Form(id string) (*form.Template, bool) {
	t, ok := p.forms[id]
	if !ok {
		return nil, false
	}
	snapshot := *t
	return &snapshot, true
}
