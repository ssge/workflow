package model

import (
	"encoding/xml"
	"fmt"
	"slices"
	"strings"
)

const (
	bpmnNS    = "http://www.omg.org/spec/BPMN/20100524/MODEL"
	bpmnDI    = "http://www.omg.org/spec/BPMN/20100524/DI"
	zeebeNS   = "http://camunda.org/schema/zeebe/1.0"
	modelerNS = "http://camunda.org/schema/modeler/1.0"
)

func bpmn(local string) xml.Name { return xml.Name{Space: bpmnNS, Local: local} }

type bpmnReader struct{ file string }

func readProcess(src Source, data *planData) (map[string]declarations, *Diagnostic) {
	root, d := readXML(src)
	if d != nil {
		return nil, d
	}
	r := bpmnReader{src.Name()}
	if root.Name != bpmn("definitions") {
		return nil, problem(InvalidModel, r.file, root.id, root.Name.Local, "expected BPMN definitions")
	}
	if d := r.container(root, root.id, "targetNamespace", "exporter", "exporterVersion", "name"); d != nil {
		return nil, d
	}

	var process *element
	processLocation := root.id
	for _, child := range root.children {
		switch child.Name {
		case bpmn("process"):
			processLocation = child.id
			executable, supplied := child.lookup("isExecutable")
			if !supplied {
				continue
			}
			switch strings.TrimSpace(executable) {
			case "true", "1":
				if process != nil {
					return nil, problem(InvalidModel, r.file, child.id, "isExecutable", "more than one executable process")
				}
				process = child
			case "false", "0":
				// Descriptive processes contribute document IDs, never flow endpoints.
			default:
				return nil, problem(InvalidModel, r.file, child.id, "isExecutable", "expected an XML boolean")
			}
		case xml.Name{Space: bpmnDI, Local: "BPMNDiagram"}:
			// Geometry is preserved in the source, not in the executable graph.
		default:
			if d := r.metadata(child, root.id); d != nil {
				return nil, d
			}
		}
	}
	if process == nil {
		return nil, problem(InvalidModel, r.file, processLocation, "isExecutable", "expected one executable process")
	}
	if d := r.container(process, process.id, "name", "isExecutable"); d != nil {
		return nil, d
	}
	data.ProcessKey, data.ProcessName = process.id, process.attr("name")
	refs := make(map[string]declarations)
	for _, e := range process.children {
		switch e.Name {
		case bpmn("startEvent"), bpmn("endEvent"), bpmn("scriptTask"), bpmn("businessRuleTask"), bpmn("userTask"), bpmn("exclusiveGateway"), bpmn("parallelGateway"), bpmn("boundaryEvent"):
			n := Node{ID: e.id, Kind: NodeKind(e.Name.Local), Name: e.attr("name")}
			decl, d := r.readNode(e, &n)
			if d != nil {
				return nil, d
			}
			data.Nodes = append(data.Nodes, n)
			refs[n.ID] = decl
		case bpmn("sequenceFlow"):
			f := SequenceFlow{ID: e.id, Name: e.attr("name"), SourceRef: e.attr("sourceRef"), TargetRef: e.attr("targetRef")}
			if d := r.container(e, e.id, "name", "sourceRef", "targetRef"); d != nil {
				return nil, d
			}
			for _, child := range e.children {
				if child.Name == bpmn("conditionExpression") {
					if f.Condition != "" {
						return nil, problem(InvalidModel, r.file, e.id, "conditionExpression", "duplicate condition")
					}
					text, d := r.formal(child, e.id)
					if d != nil {
						return nil, d
					}
					f.Condition = text
					continue
				}
				if d := r.metadata(child, e.id); d != nil {
					return nil, d
				}
			}
			data.Flows = append(data.Flows, f)
		default:
			if d := r.metadata(e, process.id); d != nil {
				return nil, d
			}
		}
	}
	return refs, nil
}

type declarations struct{ incoming, outgoing []string }

var requiredElements = map[NodeKind][]xml.Name{
	UserTask:         {zeebe("userTask"), zeebe("formDefinition")},
	ScriptTask:       {zeebe("script")},
	BusinessRuleTask: {zeebe("calledDecision")},
	BoundaryEvent:    {bpmn("timerEventDefinition")},
}

func (r bpmnReader) readNode(e *element, n *Node) (declarations, *Diagnostic) {
	var refs declarations
	attrs := []string{"name"}
	switch n.Kind {
	case ExclusiveGateway:
		n.DefaultFlow = e.attr("default")
		if _, supplied := e.lookup("default"); supplied && n.DefaultFlow == "" {
			return refs, problem(InvalidModel, r.file, e.id, "default", "empty default flow reference")
		}
		attrs = append(attrs, "default")
		fallthrough
	case ParallelGateway:
		n.GatewayDirection = e.attr("gatewayDirection")
		attrs = append(attrs, "gatewayDirection")
	case BoundaryEvent:
		n.AttachedToRef = e.attr("attachedToRef")
		attrs = append(attrs, "attachedToRef", "cancelActivity")
		if value, ok := e.lookup("cancelActivity"); ok {
			switch strings.TrimSpace(value) {
			case "true", "1":
			case "false", "0":
				return refs, problem(UnsupportedFeature, r.file, e.id, "cancelActivity", "only interrupting boundary events are supported")
			default:
				return refs, problem(InvalidModel, r.file, e.id, "cancelActivity", "expected an XML boolean")
			}
		}
	}
	if d := r.container(e, e.id, attrs...); d != nil {
		return refs, d
	}
	seen := make(map[xml.Name]bool)
	for _, child := range e.children {
		switch child.Name {
		case bpmn("extensionElements"):
			if seen[child.Name] {
				return refs, problem(InvalidModel, r.file, n.ID, "extensionElements", "duplicate extension container")
			}
			seen[child.Name] = true
			if d := r.extensions(child, n, seen); d != nil {
				return refs, d
			}
		case bpmn("timerEventDefinition"):
			if n.Kind != BoundaryEvent {
				return refs, r.unsupported(child, n.ID)
			}
			if seen[child.Name] {
				return refs, problem(InvalidModel, r.file, n.ID, "timerEventDefinition", "duplicate timer definition")
			}
			seen[child.Name] = true
			seconds, d := r.duration(child, n.ID)
			if d != nil {
				return refs, d
			}
			n.DurationSeconds = seconds
		case bpmn("incoming"), bpmn("outgoing"):
			if d := r.attributes(child, e.id); d != nil {
				return refs, d
			}
			if len(child.children) != 0 {
				return refs, r.unsupported(child.children[0], e.id)
			}
			ref := strings.TrimSpace(child.text.String())
			if child.Name.Local == "incoming" {
				refs.incoming = append(refs.incoming, ref)
			} else {
				refs.outgoing = append(refs.outgoing, ref)
			}
		default:
			if d := r.metadata(child, e.id); d != nil {
				return refs, d
			}
		}
	}
	for _, name := range requiredElements[n.Kind] {
		if !seen[name] {
			return refs, problem(InvalidModel, r.file, n.ID, name.Local, "missing required "+name.Local)
		}
	}
	return refs, nil
}

// The default for any child not handled by an executable element reader is
// rejection. Only these non-executing containers may be ignored.
func (r bpmnReader) metadata(e *element, owner string) *Diagnostic {
	switch e.Name {
	case bpmn("documentation"):
		return nil
	case bpmn("extensionElements"):
		if d := r.container(e, owner); d != nil {
			return d
		}
		for _, ext := range e.children {
			if ext.Name != (xml.Name{Space: zeebeNS, Local: "properties"}) {
				return r.unsupported(ext, owner)
			}
			if d := r.container(ext, owner); d != nil {
				return d
			}
			if len(ext.children) != 0 {
				return r.unsupported(ext.children[0], owner)
			}
		}
		return nil
	default:
		return r.unsupported(e, owner)
	}
}

func (r bpmnReader) unsupported(e *element, owner string) *Diagnostic {
	if e.id != "" {
		owner = e.id
	}
	return problem(UnsupportedFeature, r.file, owner, e.Name.Local,
		fmt.Sprintf("unsupported element {%s}%s", e.Name.Space, e.Name.Local))
}

func (r bpmnReader) container(e *element, owner string, allowed ...string) *Diagnostic {
	if strings.TrimSpace(e.text.String()) != "" {
		return problem(InvalidModel, r.file, owner, e.Name.Local, "unexpected text content")
	}
	return r.attributes(e, owner, allowed...)
}

func (r bpmnReader) attributes(e *element, owner string, allowed ...string) *Diagnostic {
	for _, a := range e.Attr {
		switch a.Name.Space {
		case "xmlns", modelerNS:
			continue
		case "":
			if a.Name.Local == "xmlns" || a.Name.Local == "id" || slices.Contains(allowed, a.Name.Local) {
				continue
			}
		}
		return problem(UnsupportedFeature, r.file, owner, a.Name.Local, "unsupported attribute on "+e.Name.Local)
	}
	return nil
}
