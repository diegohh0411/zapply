package summary

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

func TestAdapterRoutesTextAndModel(t *testing.T) {
	a := &Adapter{Model: "provider/model", Generate: func(ctx context.Context, name string, messages []Message) (string, error) {
		if name != "provider/model" || len(messages) != 2 || messages[0].Role != "system" || messages[1].Content != "evidence" {
			t.Fatalf("incorrect gateway input: %s %#v", name, messages)
		}
		return "Supported summary.", nil
	}}
	req := &model.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("evidence", genai.RoleUser)}, Config: &genai.GenerateContentConfig{SystemInstruction: genai.NewContentFromText(Instruction, genai.RoleUser)}}
	count := 0
	for response, err := range a.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatal(err)
		}
		count++
		if !response.TurnComplete || response.Content.Parts[0].Text != "Supported summary." {
			t.Fatalf("unexpected response: %#v", response)
		}
	}
	if count != 1 {
		t.Fatalf("got %d responses", count)
	}
}

func TestCancelledRunDoesNotPublishLateOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := &Adapter{Model: "model", Generate: func(context.Context, string, []Message) (string, error) { cancel(); return "stale summary", nil }}
	for response, err := range a.GenerateContent(ctx, &model.LLMRequest{}, false) {
		if response != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("published cancelled result: %v %v", response, err)
		}
	}
}

func TestNewRequiresGateway(t *testing.T) {
	if _, err := New("model", DefaultMaxOutputTokens, nil); err == nil {
		t.Fatal("agent accepted missing allowance gateway")
	}
}

func TestRunUsesFreshADKSession(t *testing.T) {
	calls := 0
	text, err := Run(context.Background(), "test/model", `{"job_description":"Write Go","sections":[]}`, DefaultMaxOutputTokens, func(ctx context.Context, name string, messages []Message) (string, error) {
		calls++
		if name != "test/model" || len(messages) < 2 || messages[len(messages)-1].Content != `{"job_description":"Write Go","sections":[]}` {
			t.Fatalf("unexpected agent input: %s %#v", name, messages)
		}
		return "Engineer with demonstrated experience building software.", nil
	})
	if err != nil || calls != 1 || text != "Engineer with demonstrated experience building software." {
		t.Fatalf("run: %q calls=%d err=%v", text, calls, err)
	}
}

func TestSummaryCharacterCapCountsUnicode(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		valid      bool
	}{
		{"at limit", strings.Repeat("é", MaxCharacters), true},
		{"over limit", strings.Repeat("é", MaxCharacters+1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Adapter{Model: "test/model", Generate: func(context.Context, string, []Message) (string, error) { return tc.text, nil }}
			for response, err := range a.GenerateContent(context.Background(), &model.LLMRequest{}, false) {
				if tc.valid && (err != nil || response == nil) {
					t.Fatalf("rejected valid output: %v", err)
				}
				if !tc.valid && (err == nil || response != nil) {
					t.Fatal("accepted oversized output")
				}
			}
		})
	}
}
