package expression

import "testing"

func TestScoreRangeRequiresTruthForEveryPossibleScore(t *testing.T) {
	for _, tc := range []struct {
		text string
		want Truth
	}{
		{"score >= 100", True}, {"score > 100", Unknown}, {"score <= 200", True}, {"score < 100", False},
		{"score == 150", Unknown}, {"score != 150", Unknown}, {"score != 99", True},
		{"!(score < 100)", True}, {"score >= 100 && p.profile.x > 0.5", Unknown}, {"score >= 100 || p.profile.x > 0.5", True},
	} {
		t.Run(tc.text, func(t *testing.T) {
			p, err := Compile(tc.text, []string{"p.profile.x"}, true)
			if err != nil {
				t.Fatal(err)
			}
			if got := p.EvalScoreRange(nil, 100, 200); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}
