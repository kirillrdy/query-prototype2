package engine

import (
	"context"

	"binaryquery/format"
)

// Input-field ordering is prepared once before inference. Each bounded batch
// is filtered and projected immediately, so unlimited scans emit partial results
// and do not retain evaluation caches for every frame in the movie.
func (q Query) streamFiltered(ctx context.Context, table format.Table, selects []selection, columns []string, functions Functions, order []int, emit func(Result) error) error {
	skipped, produced := 0, 0
	for start := 0; start < len(order) && (q.limit < 0 || produced < q.limit); start += 32 {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(start+32, len(order))
		e := newEvaluation(ctx, table, functions)
		active, err := filterMatching(e, q.predicates, order[start:end])
		if err != nil {
			return err
		}
		if skipped < q.offset {
			n := min(q.offset-skipped, len(active))
			active = active[n:]
			skipped += n
		}
		if q.limit >= 0 && len(active) > q.limit-produced {
			active = active[:q.limit-produced]
		}
		for len(active) > 0 {
			batch := 32
			if produced == 0 {
				batch = 1
			} else if produced == 1 {
				batch = 8
			}
			n := min(batch, len(active))
			chunk := Result{Columns: columns, Rows: make([]format.Row, n)}
			for i := range chunk.Rows {
				chunk.Rows[i] = format.Row{}
			}
			for _, s := range selects {
				values, err := e.evaluate(s.expression, active[:n])
				if err != nil {
					return err
				}
				for i, value := range values {
					chunk.Rows[i][s.name] = value
				}
			}
			if err := emit(chunk); err != nil {
				return err
			}
			produced += n
			active = active[n:]
		}
	}
	return nil
}

func filterMatching(e *evaluation, predicates []predicate, active []int) ([]int, error) {
	for _, p := range predicates {
		left, err := e.evaluate(p.left, active)
		if err != nil {
			return nil, err
		}
		var right, upper []any
		if p.right != nil {
			right, err = e.evaluate(p.right, active)
			if err != nil {
				return nil, err
			}
		}
		if p.upper != nil {
			upper, err = e.evaluate(p.upper, active)
			if err != nil {
				return nil, err
			}
		}
		matches := make([]int, 0, len(active))
		for i, value := range left {
			var bound, high any
			if right != nil {
				bound = right[i]
			}
			if upper != nil {
				high = upper[i]
			}
			if matchesPredicate(p.op, value, bound, high) {
				matches = append(matches, active[i])
			}
		}
		active = matches
	}
	return active, nil
}
