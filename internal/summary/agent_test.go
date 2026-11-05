package summary

import (
	"context"
	"errors"
	"fmt"
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
	if _, err := New("model", DefaultMaxOutputTokens, MaxCharacters, nil); err == nil {
		t.Fatal("agent accepted missing allowance gateway")
	}
}

func TestRunUsesFreshADKSession(t *testing.T) {
	calls := 0
	text, err := Run(context.Background(), "test/model", `{"job_description":"Write Go","sections":[]}`, DefaultMaxOutputTokens, MaxCharacters, func(ctx context.Context, name string, messages []Message) (string, error) {
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

func TestRunEnforcesConfiguredCharacterCap(t *testing.T) {
	for _, cap := range []int{100, 200, 300, 400, 500} {
		for _, extra := range []int{0, 1} {
			t.Run(fmt.Sprintf("cap_%d_extra_%d", cap, extra), func(t *testing.T) {
				output := strings.Repeat("é", cap+extra)
				text, err := Run(context.Background(), "test/model", "selected evidence", DefaultMaxOutputTokens, cap, func(_ context.Context, _ string, messages []Message) (string, error) {
					if len(messages) < 2 || !strings.Contains(messages[0].Content, fmt.Sprintf("at most %d Unicode characters", cap)) {
						t.Fatalf("configured limit missing from instruction: %#v", messages)
					}
					return output, nil
				})
				if extra == 0 && (err != nil || text != output) {
					t.Fatalf("valid Unicode output rejected: %q %v", text, err)
				}
				if extra == 1 && (err == nil || text != "") {
					t.Fatalf("oversized output accepted: %q %v", text, err)
				}
			})
		}
	}
}

func TestShortSummaryPromptHasNoConflictingWordTarget(t *testing.T) {
	instruction := InstructionFor(200)
	if strings.Contains(instruction, "50–75 words") || !strings.Contains(instruction, "about 150 characters") {
		t.Fatal(instruction)
	}
}

func TestAdapterDistinguishesEmptyAndOversizedOutput(t *testing.T) {
	for _, tc := range []struct {
		text string
		want error
	}{{"   ", ErrEmptyOutput}, {strings.Repeat("é", 201), ErrCharacterLimit}} {
		a := &Adapter{Model: "test/model", MaxCharacters: 200, Generate: func(context.Context, string, []Message) (string, error) { return tc.text, nil }}
		for response, err := range a.GenerateContent(context.Background(), &model.LLMRequest{}, false) {
			if response != nil || !errors.Is(err, tc.want) {
				t.Fatalf("response=%v err=%v", response, err)
			}
		}
	}
}
