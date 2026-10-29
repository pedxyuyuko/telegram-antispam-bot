package expression

import (
	"strings"
	"testing"
)

func TestConditionsUseThreeValuedLogic(t *testing.T) {
	variables := []string{"p.message.fraud", "p.context.benign"}
	for _, tc := range []struct {
		text   string
		values map[string]float64
		score  float64
		want   Truth
	}{
		{"score >= 60 || p.message.fraud >= 0.98", nil, 60, True},
		{"score >= 60 || p.message.fraud >= 0.98", nil, 59, Unknown},
		{"score >= 60 && p.message.fraud >= 0.98", nil, 59, False},
		{"score >= 60 && p.message.fraud >= 0.98", nil, 60, Unknown},
		{"p.message.fraud >= 0.98 || score >= 60", nil, 60, True},
		{"p.message.fraud >= 0.98 && score >= 60", nil, 59, False},
		{"!(p.message.fraud > 0.5)", nil, 0, Unknown},
		{"p.message.fraud > 0.9", map[string]float64{"p.message.fraud": .9}, 0, False},
		{"p.message.fraud >= 0.9", map[string]float64{"p.message.fraud": .9}, 0, True},
		{"p.message.fraud < 0.1", map[string]float64{"p.message.fraud": 0}, 0, True},
		{"score < -5 || (p.message.fraud > 0.9 && p.context.benign < 0.2)", map[string]float64{"p.message.fraud": .95, "p.context.benign": .1}, 0, True},
		{"score == 0 && score != 1 && score <= 0", nil, 0, True},
		{"false || true && false", nil, 0, False},
	} {
		t.Run(tc.text, func(t *testing.T) {
			p, err := Compile(tc.text, variables, true)
			if err != nil {
				t.Fatal(err)
			}
			if got := p.Eval(tc.values, tc.score); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestAllTruthPairs(t *testing.T) {
	states := []map[string]float64{{"p.a": 1}, {"p.a": 0}, {}}
	ands := [][]Truth{{True, False, Unknown}, {False, False, False}, {Unknown, False, Unknown}}
	ors := [][]Truth{{True, True, True}, {True, False, Unknown}, {True, Unknown, Unknown}}
	for _, tc := range []struct {
		op   string
		want [][]Truth
	}{{"&&", ands}, {"||", ors}} {
		p, err := Compile("p.a > 0.5 "+tc.op+" p.b > 0.5", []string{"p.a", "p.b"}, false)
		if err != nil {
			t.Fatal(err)
		}
		for i, a := range states {
			for j, b := range states {
				values := map[string]float64{}
				if n, ok := a["p.a"]; ok {
					values["p.a"] = n
				}
				if n, ok := b["p.a"]; ok {
					values["p.b"] = n
				}
				if got := p.Eval(values, 0); got != tc.want[i][j] {
					t.Fatalf("%d %s %d: %s", i, tc.op, j, got)
				}
			}
		}
	}
}

func TestMissingReferencesAreVisibleEvenWhenConditionIsTrue(t *testing.T) {
	p, err := Compile("true || p.a > 0", []string{"p.a"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Missing(nil) || p.Eval(nil, 0) != True {
		t.Fatal("missing operand hidden by boolean simplification")
	}
	if p.Missing(map[string]float64{"p.a": 0}) {
		t.Fatal("zero treated as missing")
	}
}

func TestRejectsUnsafeUnknownOrIllTypedExpressions(t *testing.T) {
	for _, text := range []string{"", "p.missing > 0", "score > 0", "p.a", "1", "true > 0", "p.a && true", "f()", "p.a[0] > 1", "p.a + 1 > 2", "x := 1", "\"text\" == 1", "p.a > 1e999", "/* comment */ true", strings.Repeat("(", 100) + "true" + strings.Repeat(")", 100), strings.Repeat("true || ", 1000) + "false"} {
		if _, err := Compile(text, []string{"p.a"}, false); err == nil {
			t.Errorf("accepted %q", text)
		}
	}
}
