package dmn

import (
	"encoding/xml"
	"errors"
	"slices"
	"strings"
)

// Parse compiles a UTF-8 DMN document. file is its logical filename for
// diagnostics; it is never opened as a path. All expressions are compiled
// before any decision can be used.
func Parse(file string, source []byte, compile Compiler) (*Model, error) {
	if compile == nil {
		return nil, errors.New("dmn: an expression compiler is required")
	}
	root, err := readXML(file, source)
	if err != nil {
		return nil, err
	}
	if root.name != (xml.Name{Space: modelNS, Local: "definitions"}) {
		return nil, root.at("definitions").fail(UnsupportedFeature, "expected definitions in the R1 DMN namespace %q", modelNS)
	}
	if err := root.check("name namespace exporter exporterVersion expressionLanguage typeLanguage", "decision"); err != nil {
		return nil, err
	}
	elements := root.named("decision")
	if len(elements) == 0 {
		return nil, root.at("decision").fail(InvalidModel, "at least one decision is required")
	}
	model := &Model{decisions: make(map[string]*Decision, len(elements))}
	for _, e := range elements {
		d, err := compile.decision(e)
		if err != nil {
			return nil, err
		}
		model.decisions[d.id] = d
	}
	return model, nil
}

func (compile Compiler) decision(e *element) (*Decision, error) {
	if err := e.check("name", "decisionTable"); err != nil {
		return nil, err
	}
	id := e.attr("id")
	if strings.TrimSpace(id) == "" {
		return nil, e.at("id").fail(InvalidModel, "decision ID is required")
	}
	tables := e.named("decisionTable")
	if len(tables) != 1 {
		return nil, e.at("decisionTable").fail(InvalidModel, "expected exactly one decisionTable")
	}
	table := tables[0]
	if err := table.check("hitPolicy", "input", "output", "rule"); err != nil {
		return nil, err
	}
	switch policy := table.attr("hitPolicy"); policy {
	case "FIRST":
	case "":
		return nil, table.at("hitPolicy").fail(InvalidModel, "explicit hitPolicy FIRST is required")
	default:
		return nil, table.at("hitPolicy").fail(UnsupportedFeature, "hitPolicy %q is not supported; only FIRST is supported", policy)
	}
	inputs, outputs := table.named("input"), table.named("output")
	if len(inputs) == 0 {
		return nil, table.at("input").fail(InvalidModel, "at least one input is required")
	}
	if len(outputs) != 1 {
		return nil, table.at("output").fail(InvalidModel, "expected exactly one output")
	}

	d := &Decision{id: id, name: e.attr("name")}
	for _, element := range inputs {
		in, err := compile.input(element)
		if err != nil {
			return nil, err
		}
		d.inputs = append(d.inputs, in)
	}
	if err := compile.output(d, outputs[0]); err != nil {
		return nil, err
	}
	for _, element := range table.named("rule") {
		r, err := compile.rule(element, d.inputs)
		if err != nil {
			return nil, err
		}
		d.rules = append(d.rules, r)
	}
	return d, nil
}

func (compile Compiler) input(e *element) (input, error) {
	if err := e.check("label", "inputExpression"); err != nil {
		return input{}, err
	}
	exprs := e.named("inputExpression")
	if len(exprs) != 1 {
		return input{}, e.at("inputExpression").fail(InvalidModel, "expected exactly one inputExpression")
	}
	e = exprs[0]
	source, err := e.expressionText("typeRef expressionLanguage", false)
	if err != nil {
		return input{}, err
	}
	typ, err := e.columnType()
	if err != nil {
		return input{}, err
	}
	expr, err := compile.expression(e.at("inputExpression"), source)
	return input{typ: typ, expr: expr}, err
}

func (compile Compiler) output(d *Decision, e *element) error {
	if err := e.check("name label typeRef", "outputValues"); err != nil {
		return err
	}
	d.outputName = e.attr("name")
	if strings.TrimSpace(d.outputName) == "" {
		return e.at("name").fail(InvalidModel, "output name is required")
	}
	typ, err := e.columnType()
	if err != nil {
		return err
	}
	d.outputType = typ
	values := e.named("outputValues")
	switch len(values) {
	case 0:
		return nil
	case 1:
		d.allowed, err = compile.outputValues(values[0], typ)
		return err
	default:
		return e.at("outputValues").fail(InvalidModel, "expected at most one outputValues")
	}
}

func (compile Compiler) rule(e *element, inputs []input) (rule, error) {
	if err := e.check("", "inputEntry", "outputEntry"); err != nil {
		return rule{}, err
	}
	tests, results := e.named("inputEntry"), e.named("outputEntry")
	if len(tests) != len(inputs) {
		return rule{}, e.at("inputEntry").fail(InvalidModel, "expected %d input entries, got %d", len(inputs), len(tests))
	}
	if len(results) != 1 {
		return rule{}, e.at("outputEntry").fail(InvalidModel, "expected 1 output entry, got %d", len(results))
	}
	var r rule
	for i, entry := range tests {
		test, err := compile.inputEntry(entry, inputs[i].typ)
		if err != nil {
			return rule{}, err
		}
		r.tests = append(r.tests, test)
	}
	result := results[0]
	source, err := result.expressionText("expressionLanguage", false)
	if err != nil {
		return rule{}, err
	}
	r.output, err = compile.expression(result.at("outputEntry"), source)
	return r, err
}

func (compile Compiler) expression(at location, source string) (compiledExpression, error) {
	if strings.HasPrefix(source, "=") {
		return compiledExpression{}, at.fail(InvalidModel, "DMN FEEL expressions do not start with an '=' marker")
	}
	eval, err := compile(source)
	if err != nil {
		diagnostic := at.fail(InvalidModel, "invalid FEEL expression: %v", err)
		diagnostic.cause = err
		return compiledExpression{}, diagnostic
	}
	if eval == nil {
		return compiledExpression{}, at.fail(InvalidModel, "FEEL compiler returned no expression")
	}
	return compiledExpression{at: at, eval: eval}, nil
}

func (e *element) columnType() (scalarType, error) {
	switch typ := scalarType(e.attr("typeRef")); typ {
	case numberType, stringType, booleanType:
		return typ, nil
	case "":
		return "", e.at("typeRef").fail(InvalidModel, "typeRef is required")
	default:
		return "", e.at("typeRef").fail(UnsupportedFeature, "typeRef %q is not supported; expected number, string, or boolean", typ)
	}
}

func (e *element) expressionText(attrs string, allowEmpty bool) (string, error) {
	if err := e.check(attrs, "text"); err != nil {
		return "", err
	}
	at := e.at(e.name.Local)
	text := e.named("text")
	if len(text) > 1 {
		return "", at.fail(InvalidModel, "expected a single text element")
	}
	if len(text) == 1 {
		if err := text[0].check(""); err != nil {
			return "", err
		}
		if source := strings.TrimSpace(text[0].body.String()); source != "" {
			return source, nil
		}
	}
	if allowEmpty {
		return "", nil
	}
	return "", at.fail(InvalidModel, "nonempty expression text is required")
}

// check is an allowlist for the executable structure. Metadata cannot
// supply children or attributes for any of the executable fields.
func (e *element) check(attrs string, children ...string) error {
	fields := strings.Fields(attrs)
	for _, a := range e.attrs {
		name, value := a.Name, a.Value
		switch {
		case namespaceDeclaration(name):
		case name.Space == modelerNS && (name.Local == "executionPlatform" || name.Local == "executionPlatformVersion"):
		case name.Space == layoutNS && name.Local == "width":
		case name.Space == "" && (name.Local == "id" || slices.Contains(fields, name.Local)):
			if (name.Local == "expressionLanguage" || name.Local == "typeLanguage") && value != feelNS {
				return e.at(name.Local).fail(UnsupportedFeature, "only the FEEL language %q is supported, got %q", feelNS, value)
			}
		default:
			return e.at(name.Local).fail(UnsupportedFeature, "attribute %q in namespace %q is not supported", name.Local, name.Space)
		}
	}
	if e.name.Local != "text" && strings.TrimSpace(e.body.String()) != "" {
		return e.at(e.name.Local).fail(InvalidModel, "unexpected text in %s", e.name.Local)
	}
	for _, child := range e.children {
		name := child.name
		switch {
		case e.name.Local == "definitions" && name == (xml.Name{Space: diagramNS, Local: "DMNDI"}):
		case name.Space != modelNS:
			return child.at(name.Local).fail(UnsupportedFeature, "element %q in namespace %q is not supported", name.Local, name.Space)
		case slices.Contains(children, name.Local):
		case e.name.Local != "text" && name.Local == "documentation":
		case e.name.Local == "decisionTable" && name.Local == "annotation":
		case e.name.Local == "rule" && name.Local == "annotationEntry":
		case e.name.Local != "text" && name.Local == "extensionElements":
			if len(child.children) != 0 || strings.TrimSpace(child.body.String()) != "" {
				return child.at(name.Local).fail(UnsupportedFeature, "extensionElements must be empty")
			}
			for _, attr := range child.attrs {
				if !namespaceDeclaration(attr.Name) && attr.Name != (xml.Name{Local: "id"}) {
					return child.at(name.Local).fail(UnsupportedFeature, "extensionElements must be empty")
				}
			}
		default:
			return child.at(name.Local).fail(UnsupportedFeature, "element %q in %s is not supported", name.Local, e.name.Local)
		}
	}
	return nil
}

func namespaceDeclaration(name xml.Name) bool {
	return name.Space == "xmlns" || name == (xml.Name{Local: "xmlns"})
}
