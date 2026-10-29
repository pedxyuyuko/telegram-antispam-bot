package decision

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"telegram-antispam-bot/internal/config"
)

func TestSystemOneNoulContractAndFullEvidence(t *testing.T) {
	t.Setenv("TEST_MODEL_KEY", "test-key")
	var payload map[string]json.RawMessage
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("bad request: %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"m","answers":{"spam":{"type":"noul","noul":0.93}},"usage":{"input_tokens":17},"extra":"keep"}`))
	}))
	defer s.Close()
	c := &Client{HTTP: s.Client()}
	result, err := c.Evaluate(context.Background(), config.Model{BaseURL: s.URL, Name: "m", APIKeyEnv: "TEST_MODEL_KEY", TimeoutSeconds: 2}, map[string]config.Question{"spam": {Type: "noul", Instructions: "spam?"}}, json.RawMessage(`{"text":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.Probabilities["spam"] != .93 || !strings.Contains(string(result.Response), `"extra":"keep"`) {
		t.Fatalf("lost evidence: %+v", result)
	}
	if len(payload) != 3 || string(payload["model"]) != `"m"` || !strings.Contains(string(result.Request), `"text":"hello"`) {
		t.Fatalf("wrong protocol: %s", result.Request)
	}
	if strings.Contains(string(result.Request), "test-key") {
		t.Fatal("credential leaked into evidence")
	}
}

func TestInvalidResponsesAreErrorsWithPreservedBodies(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","answers":{}}`,
		`{"model":"wrong","answers":{"spam":{"type":"noul","noul":0.9}}}`,
		`{"model":"m","answers":{"spam":{"type":"noul"}}}`,
		`{"model":"m","answers":{"spam":{"type":"noul","noul":1.1}}}`,
		`{"model":"m","answers":{"spam":{"type":"noul","noul":-0.1}}}`,
		`{"model":"m","answers":{"spam":{"type":"choice","noul":0.9}}}`,
		`not json`,
	} {
		t.Run(body, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
			defer s.Close()
			result, err := (&Client{HTTP: s.Client()}).Evaluate(context.Background(), config.Model{BaseURL: s.URL, Name: "m", TimeoutSeconds: 2}, map[string]config.Question{"spam": {Type: "noul", Instructions: "spam?"}}, json.RawMessage(`{}`))
			if err == nil || string(result.Response) != body {
				t.Fatalf("invalid response accepted or discarded: %v %s", err, result.Response)
			}
		})
	}
}

func TestHTTPFailureAndCancellationAreObservable(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503); w.Write([]byte("offline")) }))
	defer s.Close()
	c := &Client{HTTP: s.Client()}
	cfg := config.Model{BaseURL: s.URL, Name: "m", TimeoutSeconds: 2}
	q := map[string]config.Question{"spam": {Type: "noul", Instructions: "spam?"}}
	result, err := c.Evaluate(context.Background(), cfg, q, json.RawMessage(`{}`))
	if err == nil || string(result.Response) != "offline" {
		t.Fatalf("503 lost: %v %+v", err, result)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Evaluate(ctx, cfg, q, json.RawMessage(`{}`)); err == nil {
		t.Fatal("cancellation ignored")
	}
}

func TestTypedAnswersPreserveServerConfidenceAndQuestionCriteria(t *testing.T) {
	questions := map[string]config.Question{
		"message.fraud":    {Type: "noul", Instructions: "fraud?", Criteria: json.RawMessage(`{"true":"scam","false":"chat"}`)},
		"message.kind":     {Type: "choice", Instructions: "kind?", Criteria: json.RawMessage(`{"spam":"promotion","ham":"chat"}`)},
		"message.severity": {Type: "score", Instructions: "severity?", Criteria: json.RawMessage(`["none","low","high"]`)},
	}
	body := `{"model":"m","answers":{"message.fraud":{"type":"noul","noul":0.98},"message.kind":{"type":"choice","choice":"spam","probabilities":{"spam":0.9,"ham":0.1},"confidence":0.2},"message.severity":{"type":"score","score":1.8,"probabilities":{"0":0.05,"1":0.1,"2":0.85}}}}`
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Questions map[string]config.Question `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if len(payload.Questions) != 3 || !reflect.DeepEqual(payload.Questions["message.fraud"], questions["message.fraud"]) {
			t.Error("criteria or grouped names lost")
		}
		w.Write([]byte(body))
	}))
	defer s.Close()
	result, err := (&Client{HTTP: s.Client()}).Evaluate(context.Background(), config.Model{BaseURL: s.URL, Name: "m", TimeoutSeconds: 2}, questions, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.Probabilities["message.fraud"] != .98 || result.Answers["message.fraud"].Confidence != nil {
		t.Fatal("noul probability was replaced by inferred confidence")
	}
	if c := result.Answers["message.kind"].Confidence; c == nil || *c != .2 {
		t.Fatal("server confidence was overwritten")
	}
	if result.Answers["message.severity"].Confidence != nil || *result.Answers["message.severity"].Score != 1.8 || string(result.Response) != body {
		t.Fatal("missing confidence or raw evidence lost")
	}
}

func TestMalformedTypedAnswersAreFailures(t *testing.T) {
	for _, tc := range []struct{ typ, criteria, answer string }{
		{"choice", `{"a":"A","b":"B"}`, `{"type":"choice","choice":"a","probabilities":{"a":0.8,"b":0.2},"confidence":1.1}`},
		{"choice", `{"a":"A","b":"B"}`, `{"type":"choice","choice":"x","probabilities":{"a":0.8,"b":0.2}}`},
		{"choice", `{"a":"A","b":"B"}`, `{"type":"choice","choice":"a","probabilities":{"a":0.8,"b":null}}`},
		{"choice", `{"a":"A","b":"B"}`, `{"type":"choice","choice":"a","probabilities":{"a":0.8}}`},
		{"choice", `{"a":"A","b":"B"}`, `{"type":"choice","choice":"a","probabilities":{"a":0.8,"b":0.8}}`},
		{"choice", `{"a":"A","b":"B"}`, `{"type":"choice","choice":"b","probabilities":{"a":0.8,"b":0.2}}`},
		{"score", `["none","high"]`, `{"type":"score","score":2,"probabilities":{"0":0.2,"1":0.8}}`},
		{"score", `["none","high"]`, `{"type":"score","probabilities":{"0":0.2,"1":0.8}}`},
		{"score", `["none","high"]`, `{"type":"score","score":0.2,"probabilities":{"0":0.2,"1":0.8}}`},
		{"score", `["none","high"]`, `{"type":"score","score":0.8,"probabilities":{"0":0.2,"1":0.8},"confidence":-1}`},
	} {
		t.Run(tc.answer, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(`{"model":"m","answers":{"q":` + tc.answer + `}}`))
			}))
			defer s.Close()
			_, err := (&Client{HTTP: s.Client()}).Evaluate(context.Background(), config.Model{BaseURL: s.URL, Name: "m", TimeoutSeconds: 2}, map[string]config.Question{"q": {Type: tc.typ, Instructions: "?", Criteria: json.RawMessage(tc.criteria)}}, json.RawMessage(`{}`))
			if err == nil {
				t.Fatal("malformed answer accepted")
			}
		})
	}
}

func TestNoulClientIgnoresConfidenceMetadataAndRetainsRawEvidence(t *testing.T) {
	for _, confidence := range []string{`2`, `-1`, `"unused"`, `{"value":0.2}`, `1e999`, `null`} {
		t.Run(confidence, func(t *testing.T) {
			body := `{"model":"m","answers":{"q":{"type":"noul","noul":0.9,"confidence":` + confidence + `},"kind":{"type":"choice","choice":"spam","probabilities":{"spam":0.9,"ham":0.1},"confidence":0.8}}}`
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
			defer s.Close()
			questions := map[string]config.Question{"q": {Type: "noul", Instructions: "risk?"}, "kind": {Type: "choice", Instructions: "kind?", Criteria: json.RawMessage(`{"spam":"ad","ham":"chat"}`)}}
			result, err := (&Client{HTTP: s.Client()}).Evaluate(context.Background(), config.Model{BaseURL: s.URL, Name: "m", TimeoutSeconds: 2}, questions, json.RawMessage(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			if result.Probabilities["q"] != .9 || len(result.Answers) != 2 || string(result.Response) != body {
				t.Fatal("noul metadata blocked valid answers or discarded evidence")
			}
		})
	}
}
