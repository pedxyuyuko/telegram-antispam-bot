package expression

import (
	"go/token"
	"math"
)

type rangeValue struct {
	low, high float64
	known     bool
	truth     Truth
}

// EvalScoreRange proves a condition across every possible score in the closed
// interval. Missing non-score variables retain three-valued unknown semantics.
func (p *Program) EvalScoreRange(values map[string]float64, low, high float64) Truth {
	if math.IsNaN(low) || math.IsNaN(high) || low > high {
		return Unknown
	}
	return p.root.evalRange(values, low, high).truth
}
func (n *node) evalRange(values map[string]float64, low, high float64) rangeValue {
	if n.variable != "" {
		if n.variable == "score" {
			return rangeValue{low: low, high: high, known: true}
		}
		v, ok := values[n.variable]
		return rangeValue{low: v, high: v, known: ok && !math.IsNaN(v) && !math.IsInf(v, 0)}
	}
	if n.op == 0 {
		return rangeValue{low: n.constant.number, high: n.constant.number, known: n.constant.known, truth: n.constant.truth}
	}
	l := n.left.evalRange(values, low, high)
	out := rangeValue{truth: Unknown}
	if n.op == token.NOT {
		if l.truth == True {
			out.truth = False
		} else if l.truth == False {
			out.truth = True
		}
		return out
	}
	r := n.right.evalRange(values, low, high)
	if n.op == token.LAND {
		if l.truth == False || r.truth == False {
			out.truth = False
		} else if l.truth == True && r.truth == True {
			out.truth = True
		}
		return out
	}
	if n.op == token.LOR {
		if l.truth == True || r.truth == True {
			out.truth = True
		} else if l.truth == False && r.truth == False {
			out.truth = False
		}
		return out
	}
	if !l.known || !r.known {
		return out
	}
	var yes, no bool
	switch n.op {
	case token.GEQ:
		yes = l.low >= r.high
		no = l.high < r.low
	case token.GTR:
		yes = l.low > r.high
		no = l.high <= r.low
	case token.LEQ:
		yes = l.high <= r.low
		no = l.low > r.high
	case token.LSS:
		yes = l.high < r.low
		no = l.low >= r.high
	case token.EQL:
		yes = l.low == l.high && r.low == r.high && l.low == r.low
		no = l.high < r.low || l.low > r.high
	case token.NEQ:
		no = l.low == l.high && r.low == r.high && l.low == r.low
		yes = l.high < r.low || l.low > r.high
	}
	if yes {
		out.truth = True
	} else if no {
		out.truth = False
	}
	return out
}
