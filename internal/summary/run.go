package summary

import (
	"context"
	"errors"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// Run uses a fresh session so no facts from previous selections leak into a draft.
func Run(ctx context.Context, modelName, input string, maxOutputTokens int, generate Generate) (string, error) {
	calls := 0
	a, err := New(modelName, maxOutputTokens, func(ctx context.Context, name string, messages []Message) (string, error) {
		calls++
		if calls > 1 {
			return "", errors.New("summary: call limit exceeded")
		}
		return generate(ctx, name, messages)
	})
	if err != nil {
		return "", err
	}
	sessions := session.InMemoryService()
	_, err = sessions.Create(ctx, &session.CreateRequest{AppName: "summary", UserID: "summary", SessionID: "summary"})
	if err != nil {
		return "", err
	}
	r, err := runner.New(runner.Config{AppName: "summary", Agent: a, SessionService: sessions})
	if err != nil {
		return "", err
	}
	for event, err := range r.Run(ctx, "summary", "summary", genai.NewContentFromText(input, genai.RoleUser), agent.RunConfig{}) {
		if err != nil {
			return "", err
		}
		if event.IsFinalResponse() && event.Content != nil {
			for _, p := range event.Content.Parts {
				if p.Text != "" {
					return p.Text, nil
				}
			}
		}
	}
	return "", errors.New("summary: no final response")
}
