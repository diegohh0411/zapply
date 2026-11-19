package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/diegohh0411/zapply/internal/api/gen"
	"github.com/diegohh0411/zapply/internal/posting"
)

func TestReadPostingErrors(t *testing.T) {
	srv := &server{} // no reader configured
	_, err := srv.ReadPosting(context.Background(), gen.ReadPostingRequestObject{Body: &gen.PostingReadInput{Url: "https://example.com/job"}})
	cases := []struct {
		name   string
		err    error
		status int
		body   string
	}{
		{"not configured", err, http.StatusServiceUnavailable, "not configured"},
		{"invalid url", posting.ErrInvalidURL, http.StatusUnprocessableEntity, `"field":"url"`},
		{"unreadable", &posting.UnreadableError{Reason: "the site answered 404"}, http.StatusUnprocessableEntity, "the site answered 404"},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		writeError(w, httptest.NewRequest("POST", "/v1/postings/read", nil), c.err)
		if w.Code != c.status || !strings.Contains(w.Body.String(), c.body) {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body.String())
		}
	}
}
