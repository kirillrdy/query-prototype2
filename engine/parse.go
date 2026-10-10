package engine

import (
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

type token struct {
	text   string
	quoted bool
}
type parser struct {
	tokens   []token
	i, depth int
}

var numeric = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

func Parse(sql string) (Query, error) {
	if len(sql) > 65536 {
		return Query{}, fmt.Errorf("query exceeds 65536 bytes")
	}
	tokens, err := lex(sql)
	if err != nil {
		return Query{}, err
	}
	p := parser{tokens: tokens}
	q, err := p.query()
	if err != nil {
		return q, err
	}
	p.accept(";")
	if p.i != len(tokens) {
		return q, fmt.Errorf("unexpected token %q", tokens[p.i].text)
	}
	return q, nil
}
func (p *parser) peek(s string) bool {
	return p.i < len(p.tokens) && !p.tokens[p.i].quoted && strings.EqualFold(p.tokens[p.i].text, s)
}
func (p *parser) accept(s string) bool {
	if p.peek(s) {
		p.i++
		return true
	}
	return false
}
func (p *parser) take(s string) error {
	if !p.accept(s) {
		return fmt.Errorf("expected %s at token %d", s, p.i+1)
	}
	return nil
}
func (p *parser) identifier() (string, error) {
	if p.i >= len(p.tokens) || p.tokens[p.i].quoted || !isIdentifier(p.tokens[p.i].text) {
		return "", fmt.Errorf("expected identifier at token %d", p.i+1)
	}
	s := strings.ToLower(p.tokens[p.i].text)
	p.i++
	return s, nil
}
func (p *parser) query() (Query, error) {
	p.depth++
	defer func() { p.depth-- }()
	q := Query{limit: -1}
	if p.depth > 32 {
		return q, fmt.Errorf("query nesting exceeds 32 levels")
	}
	if err := p.take("SELECT"); err != nil {
		return q, err
	}
	if p.accept("*") {
		q.star = true
	} else {
		seen := map[string]bool{}
		for {
			e, err := p.expression(0)
			if err != nil {
				return q, err
			}
			name := e.label()
			if p.accept("AS") {
				name, err = p.identifier()
				if err != nil {
					return q, err
				}
			}
			if seen[name] {
				return q, fmt.Errorf("duplicate selected column %q", name)
			}
			seen[name] = true
			q.selects = append(q.selects, selection{e, name})
			if !p.accept(",") {
				break
			}
		}
	}
	if err := p.take("FROM"); err != nil {
		return q, err
	}
	if p.accept("(") {
		inner, err := p.query()
		if err != nil {
			return q, err
		}
		q.subquery = &inner
		if err := p.take(")"); err != nil {
			return q, err
		}
		p.accept("AS")
		if _, err := p.identifier(); err != nil {
			return q, fmt.Errorf("derived table requires an alias: %w", err)
		}
	} else {
		var err error
		q.table, err = p.identifier()
		if err != nil {
			return q, err
		}
	}
	if p.accept("WHERE") {
		for {
			left, err := p.expression(0)
			if err != nil {
				return q, err
			}
			pred := predicate{left: left}
			if p.accept("IS") {
				pred.op = "IS NULL"
				if p.accept("NOT") {
					pred.op = "IS NOT NULL"
				}
				if err := p.take("NULL"); err != nil {
					return q, err
				}
			} else if p.peek("BETWEEN") || p.peek("NOT") {
				pred.op = "BETWEEN"
				if p.accept("NOT") {
					pred.op = "NOT BETWEEN"
				}
				if err := p.take("BETWEEN"); err != nil {
					return q, err
				}
				pred.right, err = p.expression(0)
				if err != nil {
					return q, err
				}
				if err := p.take("AND"); err != nil {
					return q, err
				}
				pred.upper, err = p.expression(0)
				if err != nil {
					return q, err
				}
			} else {
				if p.i >= len(p.tokens) {
					return q, fmt.Errorf("missing comparison operator")
				}
				pred.op = p.tokens[p.i].text
				if p.tokens[p.i].quoted || !strings.Contains("|=|!=|<>|<|<=|>|>=|", "|"+pred.op+"|") {
					return q, fmt.Errorf("unsupported operator %q", pred.op)
				}
				p.i++
				pred.right, err = p.expression(0)
				if err != nil {
					return q, err
				}
			}
			q.predicates = append(q.predicates, pred)
			if !p.accept("AND") {
				break
			}
		}
	}
	if p.accept("ORDER") {
		if err := p.take("BY"); err != nil {
			return q, err
		}
		var err error
		q.order, err = p.orderTerms()
		if err != nil {
			return q, err
		}
	}
	seen := map[string]bool{}
	for p.peek("LIMIT") || p.peek("OFFSET") {
		clause := strings.ToUpper(p.tokens[p.i].text)
		if seen[clause] {
			return q, fmt.Errorf("duplicate %s clause", clause)
		}
		seen[clause] = true
		p.i++
		if p.i >= len(p.tokens) || p.tokens[p.i].quoted {
			return q, fmt.Errorf("missing %s integer", clause)
		}
		n, err := strconv.Atoi(p.tokens[p.i].text)
		if err != nil || n < 0 {
			return q, fmt.Errorf("%s must be a nonnegative integer", clause)
		}
		p.i++
		if clause == "LIMIT" {
			q.limit = n
		} else {
			q.offset = n
		}
	}
	return q, nil
}
func (p *parser) expression(depth int) (*expression, error) {
	if depth > 32 {
		return nil, fmt.Errorf("expression nesting exceeds 32 levels")
	}
	if p.i >= len(p.tokens) {
		return nil, fmt.Errorf("missing expression")
	}
	t := p.tokens[p.i]
	if t.quoted {
		p.i++
		return &expression{kind: "literal", value: t.text}, nil
	}
	if p.accept("NULL") {
		return &expression{kind: "literal"}, nil
	}
	if p.accept("TRUE") {
		return &expression{kind: "literal", value: true}, nil
	}
	if p.accept("FALSE") {
		return &expression{kind: "literal", value: false}, nil
	}
	if numeric.MatchString(t.text) {
		v, err := number(t.text)
		if err != nil {
			return nil, err
		}
		p.i++
		return &expression{kind: "literal", value: v}, nil
	}
	name, err := p.identifier()
	if err != nil {
		return nil, err
	}
	e := &expression{kind: "column", name: name}
	if !p.accept("(") {
		return e, nil
	}
	e.kind = "call"
	if !p.accept(")") {
		for {
			if len(e.args) >= 16 {
				return nil, fmt.Errorf("function exceeds 16 arguments")
			}
			a, err := p.expression(depth + 1)
			if err != nil {
				return nil, err
			}
			e.args = append(e.args, a)
			if !p.accept(",") {
				break
			}
		}
		if err := p.take(")"); err != nil {
			return nil, err
		}
	}
	if name == "lag" {
		if len(e.args) < 1 || len(e.args) > 3 {
			return nil, fmt.Errorf("LAG expects value, optional offset, and optional default")
		}
		e.kind = "lag"
		if err := p.take("OVER"); err != nil {
			return nil, err
		}
		if err := p.take("("); err != nil {
			return nil, err
		}
		w := &window{}
		if p.accept("PARTITION") {
			if err := p.take("BY"); err != nil {
				return nil, err
			}
			for {
				c, err := p.identifier()
				if err != nil {
					return nil, err
				}
				w.partition = append(w.partition, c)
				if !p.accept(",") {
					break
				}
			}
		}
		if p.accept("ORDER") {
			if err := p.take("BY"); err != nil {
				return nil, err
			}
			w.order, err = p.orderTerms()
			if err != nil {
				return nil, err
			}
		}
		if err := p.take(")"); err != nil {
			return nil, err
		}
		e.window = w
	}
	return e, nil
}
func (p *parser) orderTerms() ([]orderTerm, error) {
	var terms []orderTerm
	for {
		c, err := p.identifier()
		if err != nil {
			return nil, err
		}
		term := orderTerm{column: c}
		if p.accept("DESC") {
			term.desc = true
			term.nullsFirst = true
		} else {
			p.accept("ASC")
		}
		if p.accept("NULLS") {
			if p.accept("FIRST") {
				term.nullsFirst = true
			} else if p.accept("LAST") {
				term.nullsFirst = false
			} else {
				return nil, fmt.Errorf("expected NULLS FIRST or LAST")
			}
		}
		terms = append(terms, term)
		if !p.accept(",") {
			break
		}
	}
	return terms, nil
}
func number(s string) (any, error) {
	if len(s) > 128 {
		return nil, fmt.Errorf("numeric literal exceeds 128 bytes")
	}
	if e := strings.IndexAny(s, "eE"); e >= 0 {
		exponent, err := strconv.Atoi(s[e+1:])
		if err != nil || exponent < -400 || exponent > 400 {
			return nil, fmt.Errorf("numeric exponent must be between -400 and 400")
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid numeric literal %q", s)
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("invalid numeric literal %q", s)
	}
	if r.IsInt() {
		if r.Num().IsInt64() {
			return r.Num().Int64(), nil
		}
		if r.Num().Sign() >= 0 && r.Num().IsUint64() {
			return r.Num().Uint64(), nil
		}
	}
	return f, nil
}
func (e *expression) label() string {
	if e.kind == "column" {
		return e.name
	}
	if e.kind == "literal" {
		if e.value == nil {
			return "null"
		}
		return fmt.Sprint(e.value)
	}
	args := make([]string, len(e.args))
	for i, a := range e.args {
		args[i] = a.label()
	}
	s := e.name + "(" + strings.Join(args, ", ") + ")"
	if e.window != nil {
		s += " OVER ("
		if len(e.window.partition) > 0 {
			s += "PARTITION BY " + strings.Join(e.window.partition, ", ") + " "
		}
		if len(e.window.order) > 0 {
			terms := make([]string, len(e.window.order))
			for i, o := range e.window.order {
				terms[i] = o.column
				if o.desc {
					terms[i] += " DESC"
				}
				if o.nullsFirst != o.desc {
					if o.nullsFirst {
						terms[i] += " NULLS FIRST"
					} else {
						terms[i] += " NULLS LAST"
					}
				}
			}
			s += "ORDER BY " + strings.Join(terms, ", ")
		}
		s = strings.TrimSpace(s) + ")"
	}
	return s
}
func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		if c != '_' && !unicode.IsLetter(c) && !(i > 0 && unicode.IsDigit(c)) {
			return false
		}
	}
	return true
}
func lex(s string) ([]token, error) {
	var out []token
	for i := 0; i < len(s); {
		if strings.ContainsRune(" \n\r\t", rune(s[i])) {
			i++
			continue
		}
		if s[i] == '\'' {
			i++
			var b strings.Builder
			closed := false
			for i < len(s) {
				if s[i] == '\'' {
					i++
					if i < len(s) && s[i] == '\'' {
						b.WriteByte('\'')
						i++
						continue
					}
					closed = true
					break
				}
				b.WriteByte(s[i])
				i++
			}
			if !closed {
				return nil, fmt.Errorf("unterminated string")
			}
			out = append(out, token{b.String(), true})
			continue
		}
		start := i
		if strings.ContainsRune("(),*;=<>!", rune(s[i])) {
			i++
			if i < len(s) && (s[i] == '=' || (s[start] == '<' && s[i] == '>')) && strings.ContainsRune("<>!", rune(s[start])) {
				i++
			}
		} else {
			for i < len(s) && !strings.ContainsRune(" \t\r\n(),*;=<>!'", rune(s[i])) {
				i++
			}
		}
		out = append(out, token{text: s[start:i]})
	}
	return out, nil
}
