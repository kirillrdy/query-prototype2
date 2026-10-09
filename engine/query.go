// Package engine implements a small SQL subset over adapter-provided tables.
package engine

import (
	"context"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"unicode"

	"binaryquery/format"
)

type predicate struct {
	column, op string
	value      any
}
type Query struct {
	table      string
	columns    []string
	predicates []predicate
	limit      int
}
type Result struct {
	Columns []string     `json:"columns"`
	Rows    []format.Row `json:"rows"`
}
type token struct {
	text   string
	quoted bool
}

// Parse accepts SELECT columns FROM table [WHERE comparisons joined by AND]
// [LIMIT nonnegative-integer]. Strings use SQL single quotes and ” escaping.
func Parse(sql string) (Query, error) {
	if len(sql) > 65536 {
		return Query{}, fmt.Errorf("query exceeds 65536 bytes")
	}
	tokens, err := lex(sql)
	if err != nil {
		return Query{}, err
	}
	i := 0
	peek := func(s string) bool {
		return i < len(tokens) && !tokens[i].quoted && strings.EqualFold(tokens[i].text, s)
	}
	take := func(s string) error {
		if !peek(s) {
			return fmt.Errorf("expected %s at token %d", s, i+1)
		}
		i++
		return nil
	}
	identifier := func() (string, error) {
		if i >= len(tokens) || tokens[i].quoted || !isIdentifier(tokens[i].text) {
			return "", fmt.Errorf("expected identifier at token %d", i+1)
		}
		s := strings.ToLower(tokens[i].text)
		i++
		return s, nil
	}
	q := Query{limit: -1}
	if err := take("SELECT"); err != nil {
		return q, err
	}
	if peek("*") {
		i++
	} else {
		seen := map[string]bool{}
		for {
			c, err := identifier()
			if err != nil {
				return q, err
			}
			if seen[c] {
				return q, fmt.Errorf("duplicate selected column %q", c)
			}
			seen[c] = true
			q.columns = append(q.columns, c)
			if !peek(",") {
				break
			}
			i++
		}
	}
	if err := take("FROM"); err != nil {
		return q, err
	}
	q.table, err = identifier()
	if err != nil {
		return q, err
	}
	if peek("WHERE") {
		i++
		for {
			c, err := identifier()
			if err != nil {
				return q, err
			}
			if i >= len(tokens) {
				return q, fmt.Errorf("missing comparison operator")
			}
			op := tokens[i].text
			if tokens[i].quoted || !strings.Contains("|=|!=|<>|<|<=|>|>=|", "|"+op+"|") {
				return q, fmt.Errorf("unsupported operator %q", op)
			}
			i++
			if i >= len(tokens) {
				return q, fmt.Errorf("missing comparison value")
			}
			t := tokens[i]
			i++
			var value any
			if t.quoted {
				value = t.text
			} else if strings.EqualFold(t.text, "true") || strings.EqualFold(t.text, "false") {
				value = strings.EqualFold(t.text, "true")
			} else {
				// Store numeric literals exactly, including offsets above 2^53.
				if len(t.text) > 128 {
					return q, fmt.Errorf("numeric literal exceeds 128 bytes")
				}
				if e := strings.IndexAny(t.text, "eE"); e >= 0 {
					exponent, err := strconv.Atoi(t.text[e+1:])
					if err != nil || exponent < -400 || exponent > 400 {
						return q, fmt.Errorf("numeric exponent must be between -400 and 400")
					}
				}
				if _, err := strconv.ParseFloat(t.text, 64); err != nil {
					return q, fmt.Errorf("expected number or single-quoted string, got %q", t.text)
				}
				r, ok := new(big.Rat).SetString(t.text)
				if !ok {
					return q, fmt.Errorf("invalid numeric literal %q", t.text)
				}
				value = r
			}
			q.predicates = append(q.predicates, predicate{c, op, value})
			if !peek("AND") {
				break
			}
			i++
		}
	}
	if peek("LIMIT") {
		i++
		if i >= len(tokens) || tokens[i].quoted {
			return q, fmt.Errorf("missing LIMIT integer")
		}
		q.limit, err = strconv.Atoi(tokens[i].text)
		if err != nil || q.limit < 0 {
			return q, fmt.Errorf("LIMIT must be a nonnegative integer")
		}
		i++
	}
	if peek(";") {
		i++
	}
	if i != len(tokens) {
		return q, fmt.Errorf("unexpected token %q", tokens[i].text)
	}
	return q, nil
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
		if s[i] == ' ' || s[i] == '\n' || s[i] == '\r' || s[i] == '\t' {
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
		if strings.ContainsRune(",*;=<>!", rune(s[i])) {
			i++
			if i < len(s) && (s[i] == '=' || (s[start] == '<' && s[i] == '>')) && strings.ContainsRune("<>!", rune(s[start])) {
				i++
			}
		} else {
			for i < len(s) && !strings.ContainsRune(" \t\r\n,*;=<>!'", rune(s[i])) {
				i++
			}
		}
		out = append(out, token{text: s[start:i]})
	}
	return out, nil
}

func (q Query) Execute(ctx context.Context, tables map[string]format.Table) (Result, error) {
	t, ok := tables[q.table]
	if !ok {
		return Result{}, fmt.Errorf("unknown table %q", q.table)
	}
	known := map[string]bool{}
	for _, c := range t.Columns {
		known[c] = true
	}
	columns := q.columns
	if len(columns) == 0 {
		columns = t.Columns
	}
	for _, c := range columns {
		if !known[c] {
			return Result{}, fmt.Errorf("unknown column %q in %s", c, q.table)
		}
	}
	for _, p := range q.predicates {
		if !known[p.column] {
			return Result{}, fmt.Errorf("unknown column %q in %s", p.column, q.table)
		}
	}
	r := Result{Columns: append([]string(nil), columns...), Rows: make([]format.Row, 0)}
	for _, row := range t.Rows {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if q.limit >= 0 && len(r.Rows) >= q.limit {
			break
		}
		match := true
		for _, p := range q.predicates {
			if !matches(row[p.column], p) {
				match = false
				break
			}
		}
		if match {
			projected := format.Row{}
			for _, c := range columns {
				projected[c] = row[c]
			}
			r.Rows = append(r.Rows, projected)
		}
	}
	return r, nil
}

func matches(value any, p predicate) bool {
	if value == nil {
		return false
	}
	var cmp int
	switch literal := p.value.(type) {
	case bool:
		b, ok := value.(bool)
		if !ok {
			return false
		}
		if b == literal {
			cmp = 0
		} else if b {
			cmp = 1
		} else {
			cmp = -1
		}
	case string:
		s, ok := value.(string)
		if !ok {
			return false
		}
		cmp = strings.Compare(s, literal)
	case *big.Rat:
		switch value.(type) {
		case int, int32, int64, uint, uint32, uint64, float32, float64:
		default:
			return false
		}
		n, ok := new(big.Rat).SetString(fmt.Sprint(value))
		if !ok {
			return false
		}
		cmp = n.Cmp(literal)
	default:
		return false
	}
	switch p.op {
	case "=":
		return cmp == 0
	case "!=", "<>":
		return cmp != 0
	case "<":
		return cmp < 0
	case "<=":
		return cmp <= 0
	case ">":
		return cmp > 0
	case ">=":
		return cmp >= 0
	}
	return false
}
