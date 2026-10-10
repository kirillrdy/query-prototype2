package engine

import (
	"context"

	"binaryquery/format"
)

// Only order expressions independent of filtering can move ahead of WHERE.
// Computed order keys and windows retain the complete filtered input.
func (q Query) filterOrder(ctx context.Context, table format.Table, aliases map[string]*expression, functions Functions) ([]int, bool, error) {
	for _, term := range q.order {
		if expr := aliases[term.column]; expr != nil && expr.kind != "column" && expr.kind != "literal" {
			return nil, false, nil
		}
	}
	order := indices(table.Len())
	if len(q.order) == 0 || q.limit == 0 {
		return order, true, nil
	}
	e := newEvaluation(ctx, table, functions)
	values := make([][]any, len(q.order))
	for i, term := range q.order {
		expr := aliases[term.column]
		if expr == nil {
			expr = &expression{kind: "column", name: term.column}
		}
		var err error
		values[i], err = e.evaluate(expr, order)
		if err != nil {
			return nil, false, err
		}
	}
	if err := sortIndices(ctx, order, values, q.order); err != nil {
		return nil, false, err
	}
	return order, true, nil
}

func filterComputedLimited(ctx context.Context, table format.Table, predicates []predicate, functions Functions, order []int, limit int) (format.Table, error) {
	selected := []int{}
	for start := 0; start < len(order) && len(selected) < limit; start += 32 {
		if err := ctx.Err(); err != nil {
			return format.Table{}, err
		}
		end := min(start+32, len(order))
		e := newEvaluation(ctx, table, functions)
		active, err := filterMatching(e, predicates, order[start:end])
		if err != nil {
			return format.Table{}, err
		}
		remaining := limit - len(selected)
		if len(active) > remaining {
			active = active[:remaining]
		}
		selected = append(selected, active...)
	}
	return format.Table{Columns: table.Columns, Count: len(selected), ValueAt: func(i int, c string) any { return table.Value(selected[i], c) }}, nil
}
