package dmn

import (
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	modelNS   = "https://www.omg.org/spec/DMN/20191111/MODEL/"
	feelNS    = "https://www.omg.org/spec/DMN/20191111/FEEL/"
	diagramNS = "https://www.omg.org/spec/DMN/20191111/DMNDI/"
	modelerNS = "http://camunda.org/schema/modeler/1.0"
	layoutNS  = "http://bpmn.io/schema/dmn/biodi/2.0"
)

type element struct {
	name     xml.Name
	attrs    []xml.Attr
	children []*element
	body     strings.Builder
	file, id string // id falls back to the nearest identified ancestor
}

func (e *element) attr(name string) string {
	for _, a := range e.attrs {
		if a.Name == (xml.Name{Local: name}) {
			return a.Value
		}
	}
	return ""
}

func (e *element) at(field string) location {
	return location{file: e.file, id: e.id, field: field}
}

func (e *element) named(name string) []*element {
	var matches []*element
	for _, child := range e.children {
		if child.name == (xml.Name{Space: modelNS, Local: name}) {
			matches = append(matches, child)
		}
	}
	return matches
}

func readXML(file string, source []byte) (*element, error) {
	at := location{file: file, field: "xml"}
	if !utf8.Valid(source) {
		return nil, at.fail(InvalidModel, "expected UTF-8 XML")
	}
	source = bytes.TrimPrefix(source, []byte("\ufeff"))
	decoder := xml.NewDecoder(bytes.NewReader(source))
	ids := make(map[string]bool)
	var stack []*element
	var root *element
	for {
		offset := decoder.InputOffset()
		tok, err := decoder.Token()
		if err == io.EOF {
			if root == nil {
				return nil, at.fail(InvalidModel, "expected an XML root element")
			}
			return root, nil
		}
		if err != nil {
			return nil, at.fail(InvalidModel, "malformed XML: %v", err)
		}
		switch tok := tok.(type) {
		case xml.Directive:
			return nil, at.fail(UnsupportedFeature, "XML directives (including DTDs) are not supported")
		case xml.ProcInst:
			if offset != 0 || tok.Target != "xml" {
				return nil, at.fail(UnsupportedFeature, "XML processing instruction %q is not supported", tok.Target)
			}
		case xml.StartElement:
			e := &element{name: tok.Name, file: file, attrs: tok.Copy().Attr}
			if len(stack) > 0 {
				parent := stack[len(stack)-1]
				e.id = parent.id
				parent.children = append(parent.children, e)
			} else if root != nil {
				return nil, at.fail(InvalidModel, "expected only one XML root element")
			} else {
				root = e
			}
			if id := e.attr("id"); id != "" {
				e.id = id
			}
			seen := make(map[xml.Name]bool, len(e.attrs))
			for _, a := range e.attrs {
				if seen[a.Name] {
					return nil, e.at(a.Name.Local).fail(InvalidModel, "duplicate XML attribute %q", a.Name.Local)
				}
				seen[a.Name] = true
			}
			if id := e.attr("id"); id != "" {
				if ids[id] {
					return nil, e.at("id").fail(InvalidModel, "duplicate XML element ID %q", id)
				}
				ids[id] = true
			}
			stack = append(stack, e)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) != 0 {
				_, _ = stack[len(stack)-1].body.Write(tok)
			} else if strings.Trim(string(tok), " \t\r\n") != "" {
				return nil, at.fail(InvalidModel, "unexpected XML text outside the root element")
			}
		}
	}
}
