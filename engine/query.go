// Package engine evaluates a SQL subset over adapter-provided tables.
package engine

import (
	"binaryquery/format"
	"context"
	"fmt"
)

type Query struct {
	table      string
	subquery   *Query
	selects    []selection
	star       bool
	predicates []predicate
	order      []orderTerm
	limit      int
}
type selection struct {
	expression *expression
	name       string
}
type predicate struct {
	left, right *expression
	op          string
}
type orderTerm struct {
	column           string
	desc, nullsFirst bool
}
type expression struct {
	kind, name string
	value      any
	args       []*expression
	window     *window
}
type window struct {
	partition []string
	order     []orderTerm
}
type Result struct {
	Columns []string     `json:"columns"`
	Rows    []format.Row `json:"rows"`
}

// Function evaluates a batch of argument tuples, returning one value per tuple.
// Batch evaluation lets expensive binary functions decode distinct frames once.
type Function struct {
	Arity    int
	Evaluate func(context.Context, [][]any) ([]any, error)
}
type Functions map[string]Function

// WithMaxRows caps the final result, leaving window inputs and subqueries intact.
func (q Query) WithMaxRows(max int) Query {
	if max >= 0 && (q.limit < 0 || q.limit > max) {
		q.limit = max
	}
	return q
}
func (q Query) Execute(ctx context.Context, tables map[string]format.Table) (Result, error) {
	return q.ExecuteWith(ctx, tables, nil)
}
func (q Query) ExecuteWith(ctx context.Context, tables map[string]format.Table, functions Functions) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	var table format.Table
	if q.subquery != nil {
		r, err := q.subquery.ExecuteWith(ctx, tables, functions)
		if err != nil {
			return Result{}, err
		}
		table = format.Table{Columns: r.Columns, Rows: r.Rows}
	} else {
		var ok bool
		table, ok = tables[q.table]
		if !ok {
			return Result{}, fmt.Errorf("unknown table %q", q.table)
		}
	}
	known := map[string]bool{}
	for _, c := range table.Columns {
		known[c] = true
	}
	selects := q.selects
	if q.star {
		selects = make([]selection, len(table.Columns))
		for i, c := range table.Columns {
			selects[i] = selection{&expression{kind: "column", name: c}, c}
		}
	}
	for _, s := range selects {
		if err := validate(s.expression, known, functions, true); err != nil {
			return Result{}, err
		}
	}
	for _, p := range q.predicates {
		if err := validate(p.left, known, functions, false); err != nil {
			return Result{}, err
		}
		if p.right != nil {
			if err := validate(p.right, known, functions, false); err != nil {
				return Result{}, err
			}
		}
	}
	aliases := map[string]*expression{}
	columns := make([]string, len(selects))
	for i, s := range selects {
		aliases[s.name] = s.expression
		columns[i] = s.name
	}
	for _, o := range q.order {
		if !known[o.column] && aliases[o.column] == nil {
			return Result{}, fmt.Errorf("unknown ORDER BY column %q", o.column)
		}
	}
	rows := table.Rows
	if len(q.predicates) > 0 {
		input := newEvaluation(ctx, rows, functions)
		active := indices(len(rows))
		matched := make([]bool, len(rows))
		for i := range matched {
			matched[i] = true
		}
		for _, p := range q.predicates {
			left, err := input.evaluate(p.left, active)
			if err != nil {
				return Result{}, err
			}
			var right []any
			if p.right != nil {
				right, err = input.evaluate(p.right, active)
				if err != nil {
					return Result{}, err
				}
			}
			for i, v := range left {
				if i%1024 == 0 {
					if err := ctx.Err(); err != nil {
						return Result{}, err
					}
				}
				match := false
				if p.op == "IS NULL" {
					match = v == nil
				} else if p.op == "IS NOT NULL" {
					match = v != nil
				} else if v != nil && right[i] != nil {
					cmp, compatible := compare(v, right[i])
					match = compatible && compareOp(cmp, p.op)
				}
				matched[i] = matched[i] && match
			}
		}
		rows = make([]format.Row, 0)
		for i, row := range table.Rows {
			if matched[i] {
				rows = append(rows, row)
			}
		}
	}
	e := newEvaluation(ctx, rows, functions)
	active := indices(len(rows))
	if len(q.order) > 0 {
		values := make([][]any, len(q.order))
		for i, o := range q.order {
			expr := aliases[o.column]
			if expr == nil {
				expr = &expression{kind: "column", name: o.column}
			}
			v, err := e.evaluate(expr, active)
			if err != nil {
				return Result{}, err
			}
			values[i] = v
		}
		if err := sortIndices(ctx, active, values, q.order); err != nil {
			return Result{}, err
		}
	}
	if q.limit >= 0 && len(active) > q.limit {
		active = active[:q.limit]
	}
	result := Result{Columns: columns, Rows: make([]format.Row, len(active))}
	for i := range result.Rows {
		result.Rows[i] = format.Row{}
	}
	for _, s := range selects {
		values, err := e.evaluate(s.expression, active)
		if err != nil {
			return Result{}, err
		}
		for i, v := range values {
			result.Rows[i][s.name] = v
		}
	}
	return result, nil
}
func validate(e *expression, known map[string]bool, functions Functions, allowWindow bool) error {
	switch e.kind {
	case "column":
		if !known[e.name] {
			return fmt.Errorf("unknown column %q", e.name)
		}
	case "call":
		f, ok := functions[e.name]
		if !ok || f.Evaluate == nil {
			return fmt.Errorf("unknown function %q", e.name)
		}
		if len(e.args) != f.Arity {
			return fmt.Errorf("%s expects %d arguments", e.name, f.Arity)
		}
	case "lag":
		if !allowWindow {
			return fmt.Errorf("LAG is not allowed here; use a subquery to filter window results")
		}
		for _, c := range e.window.partition {
			if !known[c] {
				return fmt.Errorf("unknown PARTITION BY column %q", c)
			}
		}
		for _, o := range e.window.order {
			if !known[o.column] {
				return fmt.Errorf("unknown window ORDER BY column %q", o.column)
			}
		}
		allowWindow = false
	}
	for _, a := range e.args {
		if err := validate(a, known, functions, allowWindow); err != nil {
			return err
		}
	}
	return nil
}
