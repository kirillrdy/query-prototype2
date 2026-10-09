package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"binaryquery/format"
)

type evaluation struct {
	ctx       context.Context
	rows      []format.Row
	functions Functions
	cache     map[*expression]map[int]any
	windows   map[*window]*layout
}
type layout struct {
	groups      [][]int
	group, rank []int
}

func newEvaluation(ctx context.Context, rows []format.Row, functions Functions) *evaluation {
	return &evaluation{ctx: ctx, rows: rows, functions: functions, cache: map[*expression]map[int]any{}, windows: map[*window]*layout{}}
}
func indices(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

func (e *evaluation) evaluate(expr *expression, active []int) ([]any, error) {
	if err := e.ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]any, len(active))
	if len(active) == 0 {
		return out, nil
	}
	if expr.kind == "column" || expr.kind == "literal" {
		for i, row := range active {
			if i%1024 == 0 {
				if err := e.ctx.Err(); err != nil {
					return nil, err
				}
			}
			if expr.kind == "column" {
				out[i] = e.rows[row][expr.name]
			} else {
				out[i] = expr.value
			}
		}
		return out, nil
	}
	cache := e.cache[expr]
	if cache == nil {
		cache = map[int]any{}
		e.cache[expr] = cache
	}
	missing := make([]int, 0, len(active))
	seen := map[int]bool{}
	for _, row := range active {
		if _, ok := cache[row]; !ok && !seen[row] {
			missing = append(missing, row)
			seen[row] = true
		}
	}
	if len(missing) > 0 {
		var values []any
		var err error
		if expr.kind == "lag" {
			values, err = e.lag(expr, missing)
		} else {
			args := make([][]any, len(missing))
			for i := range args {
				args[i] = make([]any, len(expr.args))
			}
			for j, a := range expr.args {
				v, evalErr := e.evaluate(a, missing)
				if evalErr != nil {
					return nil, evalErr
				}
				for i, x := range v {
					args[i][j] = x
				}
			}
			values, err = e.functions[expr.name].Evaluate(e.ctx, args)
			if err != nil {
				err = fmt.Errorf("%s: %w", expr.name, err)
			}
		}
		if err != nil {
			return nil, err
		}
		if len(values) != len(missing) {
			return nil, fmt.Errorf("function %s returned %d values for %d rows", expr.name, len(values), len(missing))
		}
		for i, row := range missing {
			cache[row] = values[i]
		}
	}
	for i, row := range active {
		out[i] = cache[row]
	}
	return out, nil
}

func (e *evaluation) lag(expr *expression, active []int) ([]any, error) {
	l, err := e.layout(expr.window)
	if err != nil {
		return nil, err
	}
	offsets := make([]any, len(active))
	for i := range offsets {
		offsets[i] = int64(1)
	}
	if len(expr.args) > 1 {
		offsets, err = e.evaluate(expr.args[1], active)
		if err != nil {
			return nil, err
		}
	}
	out := make([]any, len(active))
	targets, positions, defaultRows, defaultPositions := []int{}, []int{}, []int{}, []int{}
	for i, row := range active {
		if offsets[i] == nil {
			continue
		}
		r, ok := rational(offsets[i])
		if !ok || !r.IsInt() || r.Sign() < 0 || !r.Num().IsInt64() {
			return nil, fmt.Errorf("LAG offset must be a nonnegative integer or null")
		}
		offset := r.Num().Int64()
		rank := l.rank[row]
		if offset <= int64(rank) {
			targets = append(targets, l.groups[l.group[row]][rank-int(offset)])
			positions = append(positions, i)
		} else if len(expr.args) == 3 {
			defaultRows = append(defaultRows, row)
			defaultPositions = append(defaultPositions, i)
		}
	}
	values, err := e.evaluate(expr.args[0], targets)
	if err != nil {
		return nil, err
	}
	for i, v := range values {
		out[positions[i]] = v
	}
	if len(defaultRows) > 0 {
		defaults, err := e.evaluate(expr.args[2], defaultRows)
		if err != nil {
			return nil, err
		}
		for i, v := range defaults {
			out[defaultPositions[i]] = v
		}
	}
	return out, nil
}

func (e *evaluation) layout(w *window) (*layout, error) {
	if cached := e.windows[w]; cached != nil {
		return cached, nil
	}
	l := &layout{group: make([]int, len(e.rows)), rank: make([]int, len(e.rows))}
	groups := map[string]int{}
	for i, row := range e.rows {
		if i%1024 == 0 {
			if err := e.ctx.Err(); err != nil {
				return nil, err
			}
		}
		parts := make([]string, len(w.partition))
		for j, c := range w.partition {
			key, err := scalarKey(row[c])
			if err != nil {
				return nil, fmt.Errorf("PARTITION BY %s: %w", c, err)
			}
			parts[j] = key
		}
		key, _ := json.Marshal(parts)
		group, ok := groups[string(key)]
		if !ok {
			group = len(l.groups)
			groups[string(key)] = group
			l.groups = append(l.groups, nil)
		}
		l.groups[group] = append(l.groups[group], i)
		l.group[i] = group
	}
	values := make([][]any, len(w.order))
	for j, o := range w.order {
		values[j] = make([]any, len(e.rows))
		for i, row := range e.rows {
			values[j][i] = row[o.column]
		}
	}
	for _, group := range l.groups {
		if len(w.order) > 0 {
			if err := sortIndices(e.ctx, group, values, w.order); err != nil {
				return nil, err
			}
		}
		for rank, row := range group {
			l.rank[row] = rank
		}
	}
	e.windows[w] = l
	return l, nil
}
func scalarKey(v any) (string, error) {
	if v == nil {
		return "null", nil
	}
	if n, ok := rational(v); ok {
		return "number:" + n.RatString(), nil
	}
	switch x := v.(type) {
	case string:
		return "string:" + x, nil
	case bool:
		return fmt.Sprint("bool:", x), nil
	}
	return "", fmt.Errorf("value of type %T is not scalar", v)
}
func rational(v any) (*big.Rat, bool) {
	switch v.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number:
		return new(big.Rat).SetString(fmt.Sprint(v))
	}
	return nil, false
}
func compare(a, b any) (int, bool) {
	if x, ok := rational(a); ok {
		y, ok := rational(b)
		if !ok {
			return 0, false
		}
		return x.Cmp(y), true
	}
	switch x := a.(type) {
	case string:
		y, ok := b.(string)
		if ok {
			return strings.Compare(x, y), true
		}
	case bool:
		y, ok := b.(bool)
		if ok {
			if x == y {
				return 0, true
			}
			if x {
				return 1, true
			}
			return -1, true
		}
	}
	return 0, false
}
func compareOp(cmp int, op string) bool {
	switch op {
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
func sortIndices(ctx context.Context, active []int, values [][]any, terms []orderTerm) error {
	for j, term := range terms {
		var first any
		for i, row := range active {
			if i%1024 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			v := values[j][row]
			if v == nil {
				continue
			}
			if first == nil {
				first = v
			}
			if _, ok := compare(first, v); !ok {
				return fmt.Errorf("ORDER BY %s requires compatible scalar values", term.column)
			}
		}
	}
	checks := 0
	canceled := false
	sort.SliceStable(active, func(i, j int) bool {
		checks++
		if checks%1024 == 0 && ctx.Err() != nil {
			canceled = true
		}
		if canceled {
			return false
		}
		for k, term := range terms {
			a, b := values[k][active[i]], values[k][active[j]]
			if a == nil && b == nil {
				continue
			}
			if a == nil {
				return term.nullsFirst
			}
			if b == nil {
				return !term.nullsFirst
			}
			cmp, _ := compare(a, b)
			if cmp == 0 {
				continue
			}
			if term.desc {
				return cmp > 0
			}
			return cmp < 0
		}
		return false
	})
	return ctx.Err()
}
