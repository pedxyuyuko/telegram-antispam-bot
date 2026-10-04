// Package decision implements the System One noul probability contract.
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
	"strings"
	"time"

	"telegram-antispam-bot/internal/config"
)

type Client struct{ HTTP *http.Client }

type Result struct {
	Request       []byte
	Response      []byte
	Probabilities map[string]float64
	Duration      time.Duration
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
	for id := range questions {
		var answer struct {
			Type string   `json:"type"`
			Noul *float64 `json:"noul"`
		}
		if err := json.Unmarshal(body.Answers[id], &answer); err != nil || answer.Type != "noul" || answer.Noul == nil {
			return result, fmt.Errorf("question %s: missing or invalid noul answer", id)
		}
		p := *answer.Noul
		if math.IsNaN(p) || math.IsInf(p, 0) || p < 0 || p > 1 {
			return result, fmt.Errorf("question %s: probability outside [0,1]", id)
		}
		result.Probabilities[id] = p
	}
	return result, nil
}
