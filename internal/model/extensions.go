package model

import (
	"encoding/xml"
	"strconv"
	"strings"
)

func zeebe(local string) xml.Name { return xml.Name{Space: zeebeNS, Local: local} }

func (r bpmnReader) extensions(container *element, n *Node, seen map[xml.Name]bool) *Diagnostic {
	if d := r.container(container, n.ID); d != nil {
		return d
	}
	for _, e := range container.children {
		if seen[e.Name] {
			return problem(InvalidModel, r.file, n.ID, e.Name.Local, "duplicate extension")
		}
		seen[e.Name] = true
		var attrs []string
		switch {
		case e.Name == zeebe("properties"):
		case n.Kind == StartEvent && e.Name == zeebe("ioMapping"):
			if d := r.container(e, n.ID); d != nil {
				return d
			}
			for _, out := range e.children {
				if out.Name != zeebe("output") {
					return r.unsupported(out, n.ID)
				}
				if d := r.leaf(out, n.ID, "source", "target"); d != nil {
					return d
				}
				n.OutputMappings = append(n.OutputMappings, OutputMapping{Source: out.attr("source"), Target: out.attr("target")})
			}
			continue
		case n.Kind == UserTask && e.Name == zeebe("userTask"):
		case n.Kind == UserTask && e.Name == zeebe("formDefinition"):
			attrs = []string{"formId"}
			n.FormID = e.attr("formId")
		case n.Kind == BusinessRuleTask && e.Name == zeebe("calledDecision"):
			attrs = []string{"decisionId", "resultVariable"}
			n.DecisionID, n.ResultVariable = e.attr("decisionId"), e.attr("resultVariable")
		case n.Kind == ScriptTask && e.Name == zeebe("script"):
			attrs = []string{"expression", "resultVariable"}
			n.Expression, n.ResultVariable = e.attr("expression"), e.attr("resultVariable")
		default:
			return r.unsupported(e, n.ID)
		}
		if d := r.leaf(e, n.ID, attrs...); d != nil {
			return d
		}
	}
	return nil
}

func (r bpmnReader) leaf(e *element, id string, attrs ...string) *Diagnostic {
	if d := r.container(e, id, attrs...); d != nil {
		return d
	}
	if len(e.children) > 0 {
		return r.unsupported(e.children[0], id)
	}
	return nil
}

func (r bpmnReader) duration(e *element, id string) (int64, *Diagnostic) {
	if d := r.container(e, id); d != nil {
		return 0, d
	}
	var duration string
	for _, child := range e.children {
		if child.Name != bpmn("timeDuration") {
			if d := r.metadata(child, id); d != nil {
				return 0, d
			}
			continue
		}
		if duration != "" {
			return 0, problem(InvalidModel, r.file, id, "timeDuration", "duplicate timer duration")
		}
		s, d := r.formal(child, id)
		if d != nil {
			return 0, d
		}
		duration = s
	}
	if len(duration) > 3 && strings.HasPrefix(duration, "PT") && (strings.HasSuffix(duration, "S") || strings.HasSuffix(duration, "s")) {
		digits := duration[2 : len(duration)-1]
		if strings.Trim(digits, "0123456789") == "" {
			if n, err := strconv.ParseUint(digits, 10, 31); err == nil && n > 0 {
				return int64(n), nil
			}
		}
	}
	return 0, problem(InvalidModel, r.file, id, "timeDuration", "expected fixed PT<n>S duration, 1..2147483647 seconds")
}

func (r bpmnReader) formal(e *element, id string) (string, *Diagnostic) {
	if len(e.children) > 0 {
		return "", r.unsupported(e.children[0], id)
	}
	for _, a := range e.Attr {
		switch {
		case a.Name == (xml.Name{Space: "http://www.w3.org/2001/XMLSchema-instance", Local: "type"}):
			if e.qname(a.Value) != bpmn("tFormalExpression") {
				return "", problem(UnsupportedFeature, r.file, id, "type", "expected BPMN tFormalExpression")
			}
		case a.Name.Space == "xmlns", a.Name == (xml.Name{Local: "xmlns"}), a.Name == (xml.Name{Local: "id"}), a.Name.Space == modelerNS:
		default:
			return "", problem(UnsupportedFeature, r.file, id, a.Name.Local, "unsupported formal expression attribute")
		}
	}
	text := strings.TrimSpace(e.text.String())
	if text == "" {
		return "", problem(InvalidModel, r.file, id, e.Name.Local, "empty expression")
	}
	return text, nil
}

func (e *element) qname(value string) xml.Name {
	prefix, local, found := strings.Cut(strings.TrimSpace(value), ":")
	attr := xml.Name{Space: "xmlns", Local: prefix}
	if !found {
		local = prefix
		attr = xml.Name{Local: "xmlns"}
	}
	for n := e; n != nil; n = n.parent {
		for _, a := range n.Attr {
			if a.Name == attr {
				return xml.Name{Space: a.Value, Local: local}
			}
		}
	}
	return xml.Name{Local: local}
}
