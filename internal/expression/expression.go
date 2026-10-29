// Package expression evaluates a bounded, typed subset of Go expressions.
// Expressions can only read supplied numeric values; they cannot call code.
package expression

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"strconv"
	"strings"
)

type Truth string

const (
	Unknown Truth = "unknown"
	False   Truth = "false"
	True    Truth = "true"
)

type value struct {
	boolean bool
	known   bool
	number  float64
	truth   Truth
}

type node struct {
	op          token.Token
	left, right *node
	constant    value
	variable    string
}

type Program struct {
	root *node
	refs []string
}

// Compile validates types and every reference, including unreachable branches.
// score is available only in final conditions; score terms are order-independent.
func Compile(text string, variables []string, allowScore bool) (*Program, error) {
	if len(text) == 0 || len(text) > 4096 || strings.Contains(text, "//") || strings.Contains(text, "/*") {
		return nil, fmt.Errorf("expression must be 1..4096 bytes without comments")
	}
	a, err := parser.ParseExpr(text)
	if err != nil {
		return nil, fmt.Errorf("invalid expression: %w", err)
	}
	allowed := make(map[string]bool, len(variables))
	for _, name := range variables {
		allowed[name] = true
	}
	p := &Program{}
	count := 0
	var build func(ast.Expr, int) (*node, bool, error)
	build = func(a ast.Expr, depth int) (*node, bool, error) {
		count++
		if count > 256 || depth > 64 {
			return nil, false, fmt.Errorf("expression exceeds complexity limit")
		}
		switch a := a.(type) {
		case *ast.ParenExpr:
			return build(a.X, depth+1)
		case *ast.Ident:
			if a.Name == "true" || a.Name == "false" {
				return &node{constant: value{boolean: true, known: true, truth: Truth(a.Name)}}, true, nil
			}
			if a.Name == "score" && allowScore {
				return &node{variable: "score"}, false, nil
			}
		case *ast.SelectorExpr:
			name := selector(a)
			if allowed[name] {
				p.refs = append(p.refs, name)
				return &node{variable: name}, false, nil
			}
			return nil, false, fmt.Errorf("unknown or unselected variable %q", name)
		case *ast.BasicLit:
			if a.Kind == token.INT || a.Kind == token.FLOAT {
				n, err := strconv.ParseFloat(a.Value, 64)
				if err == nil && !math.IsNaN(n) && !math.IsInf(n, 0) {
					return &node{constant: value{known: true, number: n}}, false, nil
				}
			}
		case *ast.UnaryExpr:
			child, boolean, err := build(a.X, depth+1)
			if err != nil {
				return nil, false, err
			}
			if a.Op == token.NOT && boolean {
				return &node{op: a.Op, left: child}, true, nil
			}
			if (a.Op == token.SUB || a.Op == token.ADD) && !boolean && child.variable == "" && child.op == 0 {
				if a.Op == token.SUB {
					child.constant.number = -child.constant.number
				}
				return child, false, nil
			}
		case *ast.BinaryExpr:
			left, lb, err := build(a.X, depth+1)
			if err != nil {
				return nil, false, err
			}
			right, rb, err := build(a.Y, depth+1)
			if err != nil {
				return nil, false, err
			}
			switch a.Op {
			case token.LAND, token.LOR:
				if lb && rb {
					return &node{op: a.Op, left: left, right: right}, true, nil
				}
			case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ:
				if !lb && !rb {
					return &node{op: a.Op, left: left, right: right}, true, nil
				}
			}
		}
		return nil, false, fmt.Errorf("unsupported or ill-typed expression")
	}
	root, boolean, err := build(a, 0)
	if err != nil {
		return nil, err
	}
	if !boolean {
		return nil, fmt.Errorf("condition must produce a boolean")
	}
	p.root = root
	return p, nil
}

func selector(a ast.Expr) string {
	switch a := a.(type) {
	case *ast.Ident:
		return a.Name
	case *ast.SelectorExpr:
		if parent := selector(a.X); parent != "" {
			return parent + "." + a.Sel.Name
		}
	}
	return ""
}

func (p *Program) Missing(values map[string]float64) bool {
	for _, ref := range p.refs {
		v, ok := values[ref]
		if !ok || math.IsNaN(v) || math.IsInf(v, 0) {
			return true
		}
	}
	return false
}

func (p *Program) Eval(values map[string]float64, score float64) Truth {
	return p.root.eval(values, score).truth
}

func (n *node) eval(values map[string]float64, score float64) value {
	if n.variable != "" {
		v, ok := values[n.variable]
		if n.variable == "score" {
			v, ok = score, true
		}
		return value{number: v, known: ok && !math.IsNaN(v) && !math.IsInf(v, 0)}
	}
	if n.op == 0 {
		return n.constant
	}
	l := n.left.eval(values, score)
	out := value{boolean: true, truth: Unknown}
	if n.op == token.NOT {
		if l.truth == True {
			out.truth = False
		} else if l.truth == False {
			out.truth = True
		}
		return out
	}
	r := n.right.eval(values, score)
	switch n.op {
	case token.LAND:
		if l.truth == False || r.truth == False {
			out.truth = False
		} else if l.truth == True && r.truth == True {
			out.truth = True
		}
	case token.LOR:
		if l.truth == True || r.truth == True {
			out.truth = True
		} else if l.truth == False && r.truth == False {
			out.truth = False
		}
	default:
		if !l.known || !r.known {
			return out
		}
		var yes bool
		switch n.op {
		case token.EQL:
			yes = l.number == r.number
		case token.NEQ:
			yes = l.number != r.number
		case token.LSS:
			yes = l.number < r.number
		case token.LEQ:
			yes = l.number <= r.number
		case token.GTR:
			yes = l.number > r.number
		case token.GEQ:
			yes = l.number >= r.number
		}
		out.truth = False
		if yes {
			out.truth = True
		}
	}
	return out
}
