// Package decision implements the System One typed decision contract.
package decision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"telegram-antispam-bot/internal/config"
)

type Client struct{ HTTP *http.Client }

type Result struct {
	Request       []byte
	Response      []byte
	Probabilities map[string]float64
	Answers       map[string]Answer
	Duration      time.Duration
}

type Answer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
}

func (c *Client) Evaluate(ctx context.Context, cfg config.Model, questions map[string]config.Question, state json.RawMessage) (result Result, err error) {
	start := time.Now()
	defer func() { result.Duration = time.Since(start) }()
	result.Request, err = json.Marshal(struct {
		Model     string                     `json:"model"`
		State     json.RawMessage            `json:"state"`
		Questions map[string]config.Question `json:"questions"`
	}{cfg.Name, state, questions})
	if err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.BaseURL, "/")+"/v1/systemone", bytes.NewReader(result.Request))
	if err != nil {
		return result, errors.New("invalid model endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	if key := os.Getenv(cfg.APIKeyEnv); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return result, fmt.Errorf("model request: %w", err)
	}
	defer resp.Body.Close()
	const limit = 4 << 20
	result.Response, err = io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if len(result.Response) > limit {
		result.Response = result.Response[:limit]
		return result, errors.New("model response exceeds 4 MiB")
	}
	if err != nil {
		return result, fmt.Errorf("model response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return result, fmt.Errorf("model HTTP status %d", resp.StatusCode)
	}
	var body struct {
		Model   string                     `json:"model"`
		Answers map[string]json.RawMessage `json:"answers"`
	}
	if err := json.Unmarshal(result.Response, &body); err != nil {
		return result, errors.New("model returned invalid JSON")
	}
	if body.Model != cfg.Name {
		return result, errors.New("model response name does not match request")
	}
	result.Probabilities = make(map[string]float64, len(questions))
	result.Answers = make(map[string]Answer, len(questions))
	for id, question := range questions {
		answer, err := parseAnswer(body.Answers[id], question)
		if err != nil {
			return result, fmt.Errorf("question %s: %w", id, err)
		}
		result.Answers[id] = answer
		if answer.Type == "noul" {
			result.Probabilities[id] = *answer.Noul
		}
	}
	return result, nil
}

func probability(p float64) bool { return !math.IsNaN(p) && !math.IsInf(p, 0) && p >= 0 && p <= 1 }

func parseAnswer(raw json.RawMessage, q config.Question) (Answer, error) {
	var a Answer
	if err := json.Unmarshal(raw, &a); err != nil || a.Type != q.Type {
		return a, errors.New("missing or invalid typed answer")
	}
	if a.Confidence != nil && !probability(*a.Confidence) {
		return a, errors.New("confidence outside [0,1]")
	}
	if q.Type == "noul" {
		if a.Noul == nil || !probability(*a.Noul) {
			return a, errors.New("missing noul probability or value outside [0,1]")
		}
		return a, nil
	}
	var options map[string]string
	if q.Type == "choice" {
		if err := json.Unmarshal(q.Criteria, &options); err != nil {
			return a, err
		}
		if _, ok := options[a.Choice]; !ok {
			return a, errors.New("choice is not a declared option")
		}
	} else if q.Type == "score" {
		var levels []string
		if err := json.Unmarshal(q.Criteria, &levels); err != nil {
			return a, err
		}
		options = map[string]string{}
		for i, level := range levels {
			options[strconv.Itoa(i)] = level
		}
		if a.Score == nil || math.IsNaN(*a.Score) || math.IsInf(*a.Score, 0) || *a.Score < 0 || *a.Score > float64(len(levels)-1) {
			return a, errors.New("score outside declared levels")
		}
	} else {
		return a, errors.New("unsupported answer type")
	}
	// Keep null distinct from a real zero; json.Unmarshal into float64 maps
	// otherwise silently turns a null probability into zero.
	var nullable struct {
		Probabilities map[string]*float64 `json:"probabilities"`
	}
	if err := json.Unmarshal(raw, &nullable); err != nil {
		return a, err
	}
	if len(nullable.Probabilities) != len(options) {
		return a, errors.New("probabilities do not match declared options")
	}
	var total, expected, highest float64
	for option := range options {
		p := nullable.Probabilities[option]
		if p == nil || !probability(*p) {
			return a, errors.New("missing or invalid option probability")
		}
		total += *p
		highest = math.Max(highest, *p)
		if q.Type == "score" {
			index, _ := strconv.Atoi(option)
			expected += float64(index) * *p
		}
	}
	if math.Abs(total-1) > 1e-5 {
		return a, errors.New("option probabilities must sum to one")
	}
	if q.Type == "choice" && a.Probabilities[a.Choice] < highest-1e-5 {
		return a, errors.New("choice contradicts option probabilities")
	}
	if q.Type == "score" && math.Abs(*a.Score-expected) > 1e-5 {
		return a, errors.New("score contradicts option probabilities")
	}
	return a, nil
}
