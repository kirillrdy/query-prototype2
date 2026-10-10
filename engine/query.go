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
	offset     int
}
type selection struct {
	expression *expression
	name       string
}
type predicate struct {
	left, right, upper *expression
	op                 string
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
	return q.executeWith(ctx, tables, functions, nil)
}

// Stream emits the schema, then complete result rows in order. The first row
// is evaluated separately; later rows use small batches to amortize decoding.
// Sorting, window layouts, and derived inputs finish before their dependent rows.
// A callback error stops evaluation immediately, providing backpressure.
func (q Query) Stream(ctx context.Context, tables map[string]format.Table, functions Functions, emit func(Result) error) error {
	_, err := q.executeWith(ctx, tables, functions, emit)
	return err
}

func (q Query) executeWith(ctx context.Context, tables map[string]format.Table, functions Functions, emit func(Result) error) (Result, error) {
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
		if p.upper != nil {
			if err := validate(p.upper, known, functions, false); err != nil {
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
	if emit != nil {
		if err := emit(Result{Columns: columns}); err != nil {
			return Result{}, err
		}
	}
	rows := table
	predicates := q.predicates
	// Simple scans can stop at LIMIT. Windows and sorting still see their
	// complete input, as required by SQL semantics.
	if table.ValueAt != nil && len(q.order) == 0 && q.limit >= 0 && boundedScan(selects, predicates) {
		rows, err := filterLimited(ctx, table, predicates, pagePrefix(table.Len(), q.offset, q.limit))
		if err != nil {
			return Result{}, err
		}
		table = rows
		predicates = nil
	}
	rows = table
	if len(predicates) > 0 {
		input := newEvaluation(ctx, rows, functions)
		active := indices(rows.Len())
		matched := make([]bool, rows.Len())
		for i := range matched {
			matched[i] = true
		}
		for _, p := range predicates {
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
			var upper []any
			if p.upper != nil {
				upper, err = input.evaluate(p.upper, active)
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
				var bound, high any
				if right != nil {
					bound = right[i]
				}
				if upper != nil {
					high = upper[i]
				}
				matched[i] = matched[i] && matchesPredicate(p.op, v, bound, high)
			}
		}
		selected := make([]int, 0)
		for i, match := range matched {
			if match {
				selected = append(selected, i)
			}
		}
		rows = format.Table{Columns: table.Columns, Count: len(selected), ValueAt: func(i int, c string) any { return table.Value(selected[i], c) }}
	}
	e := newEvaluation(ctx, rows, functions)
	count := rows.Len()
	if len(q.order) == 0 {
		count = pagePrefix(count, q.offset, q.limit)
	}
	active := indices(count)
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
	if q.offset >= len(active) {
		active = active[:0]
	} else {
		active = active[q.offset:]
	}
	if q.limit >= 0 && len(active) > q.limit {
		active = active[:q.limit]
	}
	result := Result{Columns: columns, Rows: make([]format.Row, 0)}
	for start := 0; start < len(active); {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		end := len(active)
		if emit != nil {
			batch := 32
			if start == 1 {
				batch = 8
			}
			if start == 0 {
				batch = 1
			}
			if end > start+batch {
				end = start + batch
			}
		}
		chunk := Result{Columns: columns, Rows: make([]format.Row, end-start)}
		for i := range chunk.Rows {
			chunk.Rows[i] = format.Row{}
		}
		for _, s := range selects {
			values, err := e.evaluate(s.expression, active[start:end])
			if err != nil {
				return Result{}, err
			}
			for i, v := range values {
				chunk.Rows[i][s.name] = v
			}
		}
		if emit != nil {
			if err := emit(chunk); err != nil {
				return Result{}, err
			}
		} else {
			result.Rows = append(result.Rows, chunk.Rows...)
		}
		start = end
	}
	return result, nil
}

func boundedScan(selects []selection, predicates []predicate) bool {
	var hasWindow func(*expression) bool
	hasWindow = func(e *expression) bool {
		if e.kind == "lag" {
			return true
		}
		for _, a := range e.args {
			if hasWindow(a) {
				return true
			}
		}
		return false
	}
	for _, s := range selects {
		if hasWindow(s.expression) {
			return false
		}
	}
	for _, p := range predicates {
		if p.left.kind != "column" && p.left.kind != "literal" {
			return false
		}
		if p.right != nil && p.right.kind != "column" && p.right.kind != "literal" {
			return false
		}
		if p.upper != nil && p.upper.kind != "column" && p.upper.kind != "literal" {
			return false
		}
	}
	return true
}

func filterLimited(ctx context.Context, table format.Table, predicates []predicate, limit int) (format.Table, error) {
	// With no filter, LIMIT needs only an indexed prefix.
	if len(predicates) == 0 {
		n := table.Len()
		if n > limit {
			n = limit
		}
		return format.Table{Columns: table.Columns, Count: n, ValueAt: table.Value}, nil
	}
	selected := make([]int, 0)
	for i := 0; i < table.Len() && len(selected) < limit; i++ {
		if i%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return format.Table{}, err
			}
		}
		value := func(e *expression) any {
			if e.kind == "column" {
				return table.Value(i, e.name)
			}
			return e.value
		}
		matched := true
		for _, p := range predicates {
			a := value(p.left)
			var b, upper any
			if p.right != nil {
				b = value(p.right)
			}
			if p.upper != nil {
				upper = value(p.upper)
			}
			if !matchesPredicate(p.op, a, b, upper) {
				matched = false
				break
			}
		}
		if matched {
			selected = append(selected, i)
		}
	}
	return format.Table{Columns: table.Columns, Count: len(selected), ValueAt: func(i int, c string) any { return table.Value(selected[i], c) }}, nil
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

// SourceTable identifies the base relation, including through derived queries.
func (q Query) SourceTable() string {
	if q.subquery != nil {
		return q.subquery.SourceTable()
	}
	return q.table
}

// pagePrefix avoids overflow even when OFFSET and LIMIT are both MaxInt.
func pagePrefix(count, offset, limit int) int {
	if limit < 0 {
		return count
	}
	if limit == 0 {
		return 0
	}
	if offset >= count || limit >= count-offset {
		return count
	}
	return offset + limit
}

func matchesPredicate(op string, value, lower, upper any) bool {
	if op == "IS NULL" {
		return value == nil
	}
	if op == "IS NOT NULL" {
		return value != nil
	}
	if op == "BETWEEN" || op == "NOT BETWEEN" {
		// Unknown comparisons remain unknown. NOT BETWEEN is true when either
		// bound comparison is known false, even if the other bound is null.
		compareBound := func(bound any, operator string) (bool, bool) {
			if value == nil || bound == nil {
				return false, false
			}
			cmp, ok := compare(value, bound)
			return ok && compareOp(cmp, operator), ok
		}
		low, lowKnown := compareBound(lower, ">=")
		high, highKnown := compareBound(upper, "<=")
		if op == "NOT BETWEEN" {
			return (lowKnown && !low) || (highKnown && !high)
		}
		return lowKnown && highKnown && low && high
	}
	if value == nil || lower == nil {
		return false
	}
	cmp, ok := compare(value, lower)
	return ok && compareOp(cmp, op)
}
