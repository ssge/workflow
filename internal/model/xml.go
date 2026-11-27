package model

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"
	"unicode"
)

// Keep expanded names, direct text, and document order. In particular do not
// use local-name-only struct tags, which would bind extension lookalikes.
type element struct {
	xml.StartElement
	id       string
	parent   *element // for QName-valued attributes such as xsi:type
	children []*element
	text     strings.Builder
}

func (e *element) attr(key string) string {
	value, _ := e.lookup(key)
	return value
}

func (e *element) lookup(key string) (string, bool) {
	for _, a := range e.Attr {
		if a.Name == (xml.Name{Local: key}) {
			return a.Value, true
		}
	}
	return "", false
}

func validID(id string) bool {
	return id != "" && strings.IndexFunc(id, unicode.IsSpace) < 0
}

func readXML(src Source) (*element, *Diagnostic) {
	// Leave Strict on, with no Entity map or CharsetReader. Directives are
	// rejected, not interpreted; there is no resolver or filesystem fallback.
	raw := xml.NewDecoder(strings.NewReader(strings.TrimPrefix(src.Content(), "\ufeff")))
	dec := xml.NewTokenDecoder(&namespaceTokens{raw: raw, bound: map[string]int{"xml": 1}})
	ids := make(map[string]bool)
	var root *element
	var stack []*element
	for {
		var owner string
		if len(stack) > 0 {
			owner = stack[len(stack)-1].id
		}
		offset := raw.InputOffset()
		token, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, problem(InvalidModel, src.Name(), owner, "xml", err.Error())
		}
		switch t := token.(type) {
		case xml.StartElement:
			e := &element{StartElement: t}
			e.id = e.attr("id")
			attrs := make(map[xml.Name]bool, len(t.Attr))
			for _, a := range t.Attr {
				if attrs[a.Name] {
					return nil, problem(InvalidModel, src.Name(), e.id, a.Name.Local, "duplicate XML attribute")
				}
				attrs[a.Name] = true
			}
			if attrs[xml.Name{Local: "id"}] {
				if !validID(e.id) {
					return nil, problem(InvalidModel, src.Name(), e.id, "id", "element ID is empty or contains whitespace")
				}
				if ids[e.id] {
					return nil, problem(InvalidModel, src.Name(), e.id, "id", "duplicate element ID")
				}
				ids[e.id] = true
			}
			if len(stack) == 0 {
				if root != nil {
					return nil, problem(InvalidModel, src.Name(), e.id, "xml", "multiple document elements")
				}
				root = e
			} else {
				parent := stack[len(stack)-1]
				e.parent = parent
				parent.children = append(parent.children, e)
			}
			stack = append(stack, e)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].text.Write(t)
			} else if strings.Trim(string(t), " \t\r\n") != "" {
				return nil, problem(InvalidModel, src.Name(), "", "xml", "text outside document element")
			}
		case xml.Directive:
			return nil, problem(UnsupportedFeature, src.Name(), owner, "xml", "DTDs and XML directives are not supported")
		case xml.ProcInst:
			if t.Target != "xml" || offset != 0 {
				return nil, problem(UnsupportedFeature, src.Name(), owner, "xml", "unexpected processing instruction")
			}
		}
	}
	if root == nil {
		return nil, problem(InvalidModel, src.Name(), "", "xml", "missing document element")
	}
	return root, nil
}

// encoding/xml expands namespaces but tolerates unbound prefixes and illegal
// bindings (e.g. a prefixed attribute bound to the empty namespace then looks
// unqualified). Check prefix scope before letting Token expand the names.
type namespaceTokens struct {
	raw    *xml.Decoder
	bound  map[string]int
	scopes [][]string
}

func (r *namespaceTokens) Token() (xml.Token, error) {
	tok, err := r.raw.RawToken()
	switch t := tok.(type) {
	case xml.StartElement:
		var scope []string
		for _, a := range t.Attr {
			var prefix string
			switch {
			case a.Name.Space == "xmlns":
				prefix = a.Name.Local
			case a.Name == (xml.Name{Local: "xmlns"}):
				// Default namespace declaration.
			default:
				continue
			}
			const xmlNS = "http://www.w3.org/XML/1998/namespace"
			const xmlnsNS = "http://www.w3.org/2000/xmlns/"
			if (prefix != "" && a.Value == "") || (prefix == "xml") != (a.Value == xmlNS) ||
				prefix == "xmlns" || a.Value == xmlnsNS || a.Value == "xmlns" {
				return nil, fmt.Errorf("invalid namespace declaration for %q", prefix)
			}
			if prefix != "" {
				r.bound[prefix]++
				scope = append(scope, prefix)
			}
		}
		r.scopes = append(r.scopes, scope)
		if p := t.Name.Space; p != "" && r.bound[p] == 0 {
			return nil, fmt.Errorf("unbound element prefix %q", p)
		}
		for _, a := range t.Attr {
			if p := a.Name.Space; p != "" && p != "xmlns" && r.bound[p] == 0 {
				return nil, fmt.Errorf("unbound attribute prefix %q", p)
			}
		}
	case xml.EndElement:
		if i := len(r.scopes) - 1; i >= 0 {
			for _, prefix := range r.scopes[i] {
				r.bound[prefix]--
			}
			r.scopes = r.scopes[:i]
		}
	}
	return tok, err
}
