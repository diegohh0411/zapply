package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/diegohh0411/zapply/internal/logx"
)

// Never log response bodies or raw transport/JSON errors: they can contain
// prompts, mail content, credentials, or provider URLs with query parameters.
func failureKind(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected_eof"
	}
	var n net.Error
	if errors.As(err, &n) && n.Timeout() {
		return "timeout"
	}
	return "other"
}

func logHTTPFailure(ctx context.Context, method, path, reason string, started time.Time, res *http.Response, bytesRead int, err error) {
	// Lookup URLs carry generation IDs; record only the endpoint path.
	attrs := []any{"method", method, "endpoint", strings.SplitN(path, "?", 2)[0], "reason", reason, "duration_ms", time.Since(started).Milliseconds(), "response_bytes", bytesRead}
	if res != nil {
		attrs = append(attrs, "http_status", res.StatusCode, "content_type", res.Header.Get("Content-Type"), "content_length", res.ContentLength,
			"provider_request_id", res.Header.Get("X-Request-ID"), "cf_ray", res.Header.Get("CF-Ray"))
	}
	if err != nil {
		attrs = append(attrs, "error_kind", failureKind(err))
	}
	logx.From(ctx).WarnContext(ctx, "openrouter HTTP failure", attrs...)
}

// logResponse records structure only, including whether an ID was absent or
// rejected locally. Do not include the rejected value or provider error text.
func logResponse(ctx context.Context, endpoint string, raw []byte, parseErr error) {
	var envelope struct {
		ID    string          `json:"id"`
		Error json.RawMessage `json:"error"`
		Usage json.RawMessage `json:"usage"`
	}
	if parseErr == nil {
		_ = json.Unmarshal(raw, &envelope)
	}
	idState := "missing"
	if parseErr != nil {
		idState = "unparsed"
	}
	if envelope.ID != "" {
		idState = "invalid"
		if generationID.MatchString(envelope.ID) {
			idState = "valid"
		}
	}
	if parseErr == nil && idState == "valid" {
		return
	}
	attrs := []any{"endpoint", endpoint, "response_bytes", len(raw), "json_valid", json.Valid(raw), "generation_id_state", idState,
		"has_error", len(envelope.Error) > 0 && string(envelope.Error) != "null", "has_usage", len(envelope.Usage) > 0 && string(envelope.Usage) != "null"}
	if parseErr != nil {
		kind := "other"
		var syntax *json.SyntaxError
		var mismatch *json.UnmarshalTypeError
		switch {
		case errors.As(parseErr, &syntax):
			kind = "syntax"
			attrs = append(attrs, "json_error_offset", syntax.Offset)
		case errors.As(parseErr, &mismatch):
			kind = "type"
			attrs = append(attrs, "json_error_offset", mismatch.Offset)
		}
		attrs = append(attrs, "parse_error_kind", kind)
	}
	logx.From(ctx).WarnContext(ctx, "openrouter generation ID unavailable", attrs...)
}
