package openrouter

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/diegohh0411/zapply/internal/logx"
	modelapi "github.com/diegohh0411/zapply/internal/model"
)

type loggingTransport func(*http.Request) (*http.Response, error)

func (f loggingTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type brokenLogBody struct{ read bool }

func (b *brokenLogBody) Read(p []byte) (int, error) {
	if b.read {
		return 0, io.ErrUnexpectedEOF
	}
	b.read = true
	return copy(p, `{"id":"gen-partial","private":"mail-secret"`), nil
}
func (*brokenLogBody) Close() error { return nil }

func TestGenerationFailureLogs(t *testing.T) {
	for _, tt := range []struct {
		name, body, want  string
		status            int
		broken, transport bool
	}{
		{name: "transport", transport: true, want: `"error_kind":"deadline_exceeded"`},
		{name: "partial_body", broken: true, want: `"error_kind":"unexpected_eof"`},
		{name: "oversized", body: strings.Repeat("x", maxBody+1), want: `"reason":"body_too_large"`},
		{name: "html_error", status: 502, body: `<html>mail-secret</html>`, want: `"http_status":502`},
		{name: "truncated_json", body: `{"id":"gen-partial","private":"mail-secret"`, want: `"parse_error_kind":"syntax"`},
		{name: "wrong_type", body: `{"id":{"private":"mail-secret"}}`, want: `"parse_error_kind":"type"`},
		{name: "missing_id", body: `{"error":{"message":"mail-secret"}}`, want: `"generation_id_state":"missing"`},
		{name: "invalid_id", body: `{"id":"mail-secret","choices":[]}`, want: `"generation_id_state":"invalid"`},
		{name: "valid_id", body: `{"id":"gen-test","choices":[{"message":{"content":"mail-secret"}}],"usage":{"cost":0}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			ctx := logx.With(context.Background(), slog.New(slog.NewJSONHandler(&logs, nil)).With("operation_id", "op-test", "attempt_id", "attempt-test"))
			c := New("test/model", "test")
			c.HTTP = &http.Client{Transport: loggingTransport(func(*http.Request) (*http.Response, error) {
				if tt.transport {
					return nil, context.DeadlineExceeded
				}
				var body io.ReadCloser = io.NopCloser(strings.NewReader(tt.body))
				if tt.broken {
					body = &brokenLogBody{}
				}
				status := tt.status
				if status == 0 {
					status = 200
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}, "X-Request-Id": []string{"request-test"}}, Body: body, ContentLength: int64(len(tt.body))}, nil
			})}
			result, err := c.Generate(ctx, "credential-secret", modelapi.Quote{Model: "test/model", InputRate: "0", OutputRate: "0", RequestRate: "0", FetchedAt: time.Now()})
			output := logs.String()
			if strings.Contains(output, "mail-secret") || strings.Contains(output, "credential-secret") {
				t.Fatalf("sensitive data logged: %s", output)
			}
			if tt.want == "" {
				if output != "" || err != nil || result.GenerationID != "gen-test" {
					t.Fatalf("successful response: %s, %v, %+v", output, err, result)
				}
				return
			}
			if !strings.Contains(output, tt.want) || !strings.Contains(output, `"operation_id":"op-test"`) || !strings.Contains(output, `"attempt_id":"attempt-test"`) {
				t.Fatalf("missing diagnostic metadata: %s", output)
			}
			if tt.status != 0 && !strings.Contains(output, `"provider_request_id":"request-test"`) {
				t.Fatalf("missing provider correlation: %s", output)
			}
			if (tt.transport || tt.broken || tt.status != 0 || tt.name == "oversized" || tt.name == "truncated_json" || tt.name == "wrong_type") && err == nil {
				t.Fatal("expected existing provider error")
			}
		})
	}
}
