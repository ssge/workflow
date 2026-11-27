package model

import (
	"encoding/json"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ssge/workflow/internal/dmn"
	"github.com/ssge/workflow/internal/evaluator"
)

// Expression is a pure FEEL program over JSON variables (exact json.Number decimals).
type Expression = dmn.Expression

// CompileFEEL receives unmarked FEEL for BPMN, DMN, and forms. The zero
// value uses the shared R1 evaluator. Injected compilers must be pure.
type Compiler struct {
	CompileFEEL func(string) (Expression, error)
}

// Compile returns either a complete plan or diagnostics, never both.
func (c Compiler) Compile(files []Source) (*Plan, []Diagnostic) {
	src, d := bpmnSource(files)
	if d != nil {
		return nil, []Diagnostic{*d}
	}
	data := planData{ProfileRevision: profileRevision, Sources: slices.Clone(files)}
	refs, d := readProcess(src, &data)
	if d != nil {
		return nil, []Diagnostic{*d}
	}
	p, d := c.finish(src.Name(), data, refs)
	if d != nil {
		return nil, []Diagnostic{*d}
	}
	return p, nil
}

func (c Compiler) finish(file string, data planData, refs map[string]declarations) (*Plan, *Diagnostic) {
	p, d := link(file, data, refs)
	if d != nil {
		return nil, d
	}
	compile := c.CompileFEEL
	if compile == nil {
		compile = evaluator.CompileDMN
	}
	if d := p.bind(file, compile); d != nil {
		return nil, d
	}
	return p, nil
}

func bpmnSource(files []Source) (Source, *Diagnostic) {
	seen := make(map[string]bool)
	var bpmn Source
	for _, f := range files {
		name := f.Name()
		if !utf8.ValidString(name) || strings.TrimSpace(name) == "" || name == "." || name == ".." ||
			strings.ContainsAny(name, `/\:`) || strings.IndexFunc(name, unicode.IsControl) >= 0 {
			return Source{}, problem(InvalidModel, name, "", "name", "expected a logical UTF-8 filename, not a path or URL")
		}
		if seen[name] {
			return Source{}, problem(InvalidModel, name, "", "name", "duplicate filename")
		}
		seen[name] = true
		if !utf8.ValidString(f.Content()) {
			return Source{}, problem(InvalidModel, name, "", "content", "source is not UTF-8")
		}
		switch {
		case strings.HasSuffix(name, ".bpmn"):
			if bpmn.Name() != "" {
				return Source{}, problem(InvalidModel, name, "", "name", "expected exactly one BPMN file")
			}
			bpmn = f
		case strings.HasSuffix(name, ".dmn"), strings.HasSuffix(name, ".form"):
			// Bound after the entire bundle and process graph have been checked.
		case name == ".process-application":
			var obj map[string]json.RawMessage
			if err := json.Unmarshal([]byte(f.Content()), &obj); err != nil || obj == nil || len(obj) != 0 {
				return Source{}, problem(InvalidModel, name, "", "content", "packaging metadata must be an empty JSON object")
			}
		default:
			return Source{}, problem(UnsupportedFeature, name, "", "name", "unsupported resource type in this profile")
		}
	}
	if bpmn.Name() == "" {
		return Source{}, problem(InvalidModel, "", "", "files", "expected exactly one BPMN file")
	}
	return bpmn, nil
}
