package decision

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
