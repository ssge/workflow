package feel

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

type tokenKind uint8

const (
	tEnd tokenKind = iota
	tLiteral
	tName
	tLeft
	tRight
	tPlus
	tEqual
	tLess
	tLessEqual
	tGreater
	tGreaterEqual
)

type token struct {
	kind  tokenKind
	text  string
	value Value
	pos   int
}

func syntax(pos int, message string) error {
	return fmt.Errorf("%w at byte %d: %s", ErrSyntax, pos, message)
}

func tokenize(source string) ([]token, error) {
	if !utf8.ValidString(source) {
		return nil, syntax(0, "source is not UTF-8")
	}
	var tokens []token
	for i := 0; i < len(source); {
		r, size := utf8.DecodeRuneInString(source[i:])
		if unicode.IsSpace(r) {
			i += size
			continue
		}
		start := i
		c := source[i]
		if isLetter(c) {
			i++
			for i < len(source) && (isLetter(source[i]) || source[i] >= '0' && source[i] <= '9') {
				i++
			}
			name := source[start:i]
			t := token{kind: tName, text: name, pos: start}
			switch name {
			case "true":
				t.kind, t.value = tLiteral, Boolean(true)
			case "false":
				t.kind, t.value = tLiteral, Boolean(false)
			case "null":
				t.kind, t.value = tLiteral, Null()
			}
			tokens = append(tokens, t)
			continue
		}
		if c == '"' {
			end, decoded, err := readString(source, i)
			if err != nil {
				return nil, syntax(start, err.Error())
			}
			v, _ := String(decoded)
			tokens = append(tokens, token{kind: tLiteral, value: v, pos: start})
			i = end
			continue
		}
		if c >= '0' && c <= '9' || c == '-' && i+1 < len(source) && source[i+1] >= '0' && source[i+1] <= '9' {
			if c == '-' {
				i++
			}
			for i < len(source) && source[i] >= '0' && source[i] <= '9' {
				i++
			}
			if i < len(source) && source[i] == '.' {
				i++
				for i < len(source) && source[i] >= '0' && source[i] <= '9' {
					i++
				}
			}
			if i < len(source) && (source[i] == 'e' || source[i] == 'E') {
				i++
				if i < len(source) && (source[i] == '+' || source[i] == '-') {
					i++
				}
				for i < len(source) && source[i] >= '0' && source[i] <= '9' {
					i++
				}
			}
			v, err := Number(source[start:i])
			if err != nil {
				return nil, syntax(start, err.Error())
			}
			tokens = append(tokens, token{kind: tLiteral, value: v, pos: start})
			continue
		}
		i++
		t := token{pos: start}
		switch c {
		case '(':
			t.kind = tLeft
		case ')':
			t.kind = tRight
		case '+':
			t.kind = tPlus
		case '=':
			t.kind = tEqual
		case '<':
			t.kind = tLess
			if i < len(source) && source[i] == '=' {
				t.kind = tLessEqual
				i++
			}
		case '>':
			t.kind = tGreater
			if i < len(source) && source[i] == '=' {
				t.kind = tGreaterEqual
				i++
			}
		default:
			return nil, syntax(start, fmt.Sprintf("unexpected character %q", r))
		}
		tokens = append(tokens, t)
	}
	return append(tokens, token{kind: tEnd, pos: len(source)}), nil
}

type node interface {
	evaluate(map[string]Value) (Value, error)
}

type literalNode struct{ value Value }

func (n literalNode) evaluate(_ map[string]Value) (Value, error) { return n.value, nil }

type nameNode struct{ name string }

func (n nameNode) evaluate(vars map[string]Value) (Value, error) {
	return vars[n.name], nil
}

type stringNode struct{ argument node }

func (n stringNode) evaluate(vars map[string]Value) (Value, error) {
	v, err := n.argument.evaluate(vars)
	if err != nil {
		return Value{}, err
	}
	var s string
	switch v.kind {
	case StringKind:
		s = v.text
	case NumberKind:
		s = v.number.String()
	case BooleanKind:
		if v.boolean {
			s = "true"
		} else {
			s = "false"
		}
	case NullKind:
		s = "null"
	default:
		return Value{}, fmt.Errorf("%w: string expects a scalar", ErrEvaluation)
	}
	return Value{kind: StringKind, text: s}, nil
}

type binaryNode struct {
	op          tokenKind
	left, right node
}

func (n binaryNode) evaluate(vars map[string]Value) (Value, error) {
	left, err := n.left.evaluate(vars)
	if err != nil {
		return Value{}, err
	}
	right, err := n.right.evaluate(vars)
	if err != nil {
		return Value{}, err
	}
	switch n.op {
	case tEqual:
		return Boolean(left.Equal(right)), nil
	case tPlus:
		if left.kind == StringKind && right.kind == StringKind {
			return Value{kind: StringKind, text: left.text + right.text}, nil
		}
		if left.kind == NumberKind && right.kind == NumberKind {
			number, err := left.number.add(right.number)
			if err != nil {
				return Value{}, fmt.Errorf("%w: %v", ErrEvaluation, err)
			}
			return Value{kind: NumberKind, number: number}, nil
		}
		return Value{}, fmt.Errorf("%w: + expects two numbers or two strings", ErrEvaluation)
	default:
		if left.kind != NumberKind || right.kind != NumberKind {
			return Value{}, fmt.Errorf("%w: ordering expects two numbers", ErrEvaluation)
		}
		cmp := left.number.compare(right.number)
		switch n.op {
		case tLess:
			return Boolean(cmp < 0), nil
		case tLessEqual:
			return Boolean(cmp <= 0), nil
		case tGreater:
			return Boolean(cmp > 0), nil
		case tGreaterEqual:
			return Boolean(cmp >= 0), nil
		}
	}
	return Value{}, fmt.Errorf("%w: invalid operator", ErrEvaluation)
}

type parser struct {
	tokens []token
	index  int
}

func (p *parser) current() token { return p.tokens[p.index] }

func (p *parser) primary() (node, error) {
	t := p.current()
	switch t.kind {
	case tLiteral:
		p.index++
		return literalNode{t.value}, nil
	case tName:
		p.index++
		if t.text == "string" && p.current().kind == tLeft {
			p.index++
			argument, err := p.comparison()
			if err != nil {
				return nil, err
			}
			if p.current().kind != tRight {
				return nil, syntax(p.current().pos, "expected )")
			}
			p.index++
			return stringNode{argument}, nil
		}
		return nameNode{t.text}, nil
	case tLeft:
		p.index++
		inner, err := p.comparison()
		if err != nil {
			return nil, err
		}
		if p.current().kind != tRight {
			return nil, syntax(p.current().pos, "expected )")
		}
		p.index++
		return inner, nil
	default:
		return nil, syntax(t.pos, "expected expression")
	}
}

func (p *parser) addition() (node, error) {
	left, err := p.primary()
	if err != nil {
		return nil, err
	}
	for p.current().kind == tPlus {
		p.index++
		right, err := p.primary()
		if err != nil {
			return nil, err
		}
		left = binaryNode{op: tPlus, left: left, right: right}
	}
	return left, nil
}

func (p *parser) comparison() (node, error) {
	left, err := p.addition()
	if err != nil {
		return nil, err
	}
	op := p.current().kind
	if op >= tEqual && op <= tGreaterEqual {
		p.index++
		right, err := p.addition()
		if err != nil {
			return nil, err
		}
		left = binaryNode{op: op, left: left, right: right}
	}
	return left, nil
}

// Expression is a compiled, immutable expression safe for concurrent evaluation.
type Expression struct{ root node }

// Compile parses unmarked FEEL, as used in DMN expressions.
func Compile(source string) (*Expression, error) {
	tokens, err := tokenize(source)
	if err != nil {
		return nil, err
	}
	p := parser{tokens: tokens}
	root, err := p.comparison()
	if err != nil {
		return nil, err
	}
	if p.current().kind != tEnd {
		return nil, syntax(p.current().pos, "unexpected token")
	}
	return &Expression{root: root}, nil
}

// CompileMarked compiles a BPMN or dynamic form expression beginning with '='.
func CompileMarked(source string) (*Expression, error) {
	source = strings.TrimSpace(source)
	if !strings.HasPrefix(source, "=") {
		return nil, syntax(0, "expected leading = marker")
	}
	return Compile(source[1:])
}

// Evaluate reads only vars; missing identifiers evaluate as null.
func (e *Expression) Evaluate(vars map[string]Value) (Value, error) {
	if e == nil || e.root == nil {
		return Value{}, fmt.Errorf("%w: missing expression", ErrEvaluation)
	}
	return e.root.evaluate(vars)
}
