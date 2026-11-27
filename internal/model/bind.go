package model

import (
	"encoding/xml"
	"strings"

	"github.com/ssge/workflow/form"
	"github.com/ssge/workflow/internal/dmn"
	"github.com/ssge/workflow/internal/feel"
)

func (p *Plan) bind(file string, compile dmn.Compiler) *Diagnostic {
	p.forms = make(map[string]*form.Template)
	p.decisions = make(map[string]*dmn.Decision)
	for _, src := range p.data.Sources {
		switch {
		case strings.HasSuffix(src.Name(), ".form"):
			t, err := form.Parse([]byte(src.Content()), func(s string) (form.Evaluator, error) {
				f, err := compile(s)
				return form.Evaluator(f), err
			})
			if err != nil {
				return resourceError(src.Name(), err)
			}
			if _, exists := p.forms[t.ID()]; exists {
				return problem(InvalidModel, src.Name(), t.ID(), "id", "duplicate form ID in bundle")
			}
			p.forms[t.ID()] = t
		case strings.HasSuffix(src.Name(), ".dmn"):
			// dmn.Model provides lookup but not enumeration. Check namespace
			// scope and index *all* decisions, including unreferenced siblings.
			root, d := readXML(src)
			if d != nil {
				return d
			}
			m, err := dmn.Parse(src.Name(), []byte(src.Content()), compile)
			if err != nil {
				return resourceError(src.Name(), err)
			}
			for _, child := range root.children {
				if child.Name != (xml.Name{Space: "https://www.omg.org/spec/DMN/20191111/MODEL/", Local: "decision"}) {
					continue
				}
				if _, exists := p.decisions[child.id]; exists {
					return problem(InvalidModel, src.Name(), child.id, "id", "duplicate decision ID in bundle")
				}
				decision, ok := m.Decision(child.id)
				if !ok {
					return problem(InvalidModel, src.Name(), child.id, "id", "decision was not compiled")
				}
				p.decisions[child.id] = decision
			}
		}
	}
	p.expressions = make(map[string]Expression)
	marked := func(id, field, source string) *Diagnostic {
		if p.expressions[source] != nil {
			return nil
		}
		text, ok := strings.CutPrefix(strings.TrimSpace(source), "=")
		if !ok || strings.TrimSpace(text) == "" {
			return problem(InvalidModel, file, id, field, "expected a marked FEEL expression")
		}
		fn, err := compile(strings.TrimSpace(text))
		if err != nil {
			return problem(InvalidModel, file, id, field, err.Error())
		}
		if fn == nil {
			return problem(InvalidModel, file, id, field, "FEEL compiler returned no expression")
		}
		p.expressions[source] = fn
		return nil
	}
	for _, n := range p.data.Nodes {
		switch n.Kind {
		case StartEvent:
			for _, m := range n.OutputMappings {
				if !feel.ValidIdentifier(m.Target) {
					return problem(InvalidModel, file, n.ID, "target", "expected a root variable name")
				}
				if d := marked(n.ID, "source", m.Source); d != nil {
					return d
				}
			}
		case ScriptTask, BusinessRuleTask:
			if !feel.ValidIdentifier(n.ResultVariable) {
				return problem(InvalidModel, file, n.ID, "resultVariable", "expected a root variable name")
			}
			if n.Kind == ScriptTask {
				if d := marked(n.ID, "expression", n.Expression); d != nil {
					return d
				}
			} else if p.decisions[n.DecisionID] == nil {
				return problem(InvalidModel, file, n.ID, "decisionId", "no such decision in this bundle: "+n.DecisionID)
			}
		case UserTask:
			if p.forms[n.FormID] == nil {
				return problem(InvalidModel, file, n.ID, "formId", "no such form in this bundle: "+n.FormID)
			}
		}
	}
	for _, f := range p.data.Flows {
		if f.Condition != "" {
			if d := marked(f.ID, "conditionExpression", f.Condition); d != nil {
				return d
			}
		}
	}
	return nil
}

func resourceError(file string, err error) *Diagnostic {
	switch e := err.(type) {
	case *dmn.Error:
		return problem(Code(e.Code), file, e.ElementID, e.Field, e.Reason)
	case *form.Error:
		field := e.Field
		if field == "" {
			field = "content"
		}
		return problem(Code(e.Code), file, e.ElementID, field, e.Reason)
	default:
		return problem(InvalidModel, file, "", "content", err.Error())
	}
}
