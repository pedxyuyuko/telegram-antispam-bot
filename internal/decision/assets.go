package decision

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type modelHTTPError struct {
	status       int
	imageFailure bool
	localLimit   bool
}

func (e *modelHTTPError) Error() string {
	if e.localLimit {
		return "model request exceeds 32 MiB"
	}
	return fmt.Sprintf("model HTTP status %d", e.status)
}

// ImageRejected allows one text-only retry for image validation/capability or
// image-expanded token limits. Model names, credentials and outages stay errors.
func ImageRejected(err error) bool {
	var problem *modelHTTPError
	return errors.As(err, &problem) && problem.imageFailure
}

func responseError(status int, body []byte, hasImages bool) error {
	e := &modelHTTPError{status: status}
	if !hasImages {
		return e
	}
	var envelope struct {
		Detail json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return e
	}
	if status == 400 || status == 413 {
		var detail string
		if json.Unmarshal(envelope.Detail, &detail) == nil && detail != "" {
			e.imageFailure = imageValidationDetail(detail)
		}
		return e
	}
	if status != 422 {
		return e
	}
	var details []struct {
		Loc     []any  `json:"loc"`
		Message string `json:"msg"`
	}
	if json.Unmarshal(envelope.Detail, &details) != nil || len(details) == 0 {
		return e
	}
	// Every reported error must be image-related before weakening the request.
	for _, detail := range details {
		if len(detail.Loc) < 2 || detail.Loc[0] != "body" {
			return e
		}
		switch detail.Loc[1] {
		case "state", "assets":
		case "questions":
			msg := strings.ToLower(detail.Message)
			if !strings.Contains(msg, "token") || !(strings.Contains(msg, "limit") || strings.Contains(msg, "max") || strings.Contains(msg, "too long")) {
				return e
			}
		default:
			return e
		}
	}
	e.imageFailure = true
	return e
}

func imageValidationDetail(detail string) bool {
	msg := strings.ToLower(detail)
	for _, configuration := range []string{"unknown model", "invalid model", "model name", "model not found", "api key", "credential", "unauthorized", "authentication"} {
		if strings.Contains(msg, configuration) {
			return false
		}
	}
	for _, image := range []string{"image", "asset", "base64", "pixel", "dimension", "width", "height", "png", "jpeg", "webp"} {
		if strings.Contains(msg, image) {
			return true
		}
	}
	return (strings.Contains(msg, "body") || strings.Contains(msg, "expanded request")) && (strings.Contains(msg, "large") || strings.Contains(msg, "limit") || strings.Contains(msg, "exceed"))
}
