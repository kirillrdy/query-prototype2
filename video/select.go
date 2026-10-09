package video

import "fmt"

// selectExpression accepts sorted decoded-frame ordinals. Coalesce adjacent
// frames into ranges and balance sparse terms: a linear sum of hundreds of eq
// calls exhausts FFmpeg's expression parser nesting limit.
func selectExpression(ordinals []int) string {
	if len(ordinals) == 0 {
		return "0"
	}
	terms := make([]string, 0, len(ordinals))
	start, end := ordinals[0], ordinals[0]
	flush := func() {
		if start == end {
			terms = append(terms, fmt.Sprintf("eq(n\\,%d)", start))
		} else {
			terms = append(terms, fmt.Sprintf("between(n\\,%d\\,%d)", start, end))
		}
	}
	for _, n := range ordinals[1:] {
		if n == end {
			continue
		}
		if n == end+1 {
			end = n
			continue
		}
		flush()
		start, end = n, n
	}
	flush()
	var balanced func([]string) string
	balanced = func(parts []string) string {
		if len(parts) == 1 {
			return parts[0]
		}
		mid := len(parts) / 2
		return "(" + balanced(parts[:mid]) + "+" + balanced(parts[mid:]) + ")"
	}
	return balanced(terms)
}
