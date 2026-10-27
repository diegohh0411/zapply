package api_test

import (
	"fmt"
	"strings"
	"testing"
)

func TestMatchAPIStartCancelAndIsolation(t *testing.T) {
	alice, bob := newServer(t)
	profile := alice.must(200, "GET", "/v1/profile", nil)
	if profile.Body["auto_match"] != true {
		t.Fatal("auto-match not enabled by default")
	}
	alice.must(200, "PUT", "/v1/profile", map[string]any{"auto_match": false}, "If-Match", profile.Header.Get("ETag"))
	saved := alice.must(201, "POST", "/v1/jobs", map[string]any{"title": "Engineer", "description": "Go experience"})
	job := saved.Body["id"].(string)
	path := "/v1/jobs/" + job + "/match-runs"
	alice.must(400, "POST", path, nil)
	started := alice.must(202, "POST", path, nil, "Idempotency-Key", "match-one")
	id := started.Body["id"].(string)
	replay := alice.must(202, "POST", path, nil, "Idempotency-Key", "match-one")
	if replay.Body["id"] != id {
		t.Fatal("idempotency replay changed run")
	}
	conflict := alice.must(409, "POST", path, nil, "Idempotency-Key", "match-two")
	if conflict.Body["existing_id"] != id {
		t.Fatal("missing existing run id")
	}
	list := alice.must(200, "GET", path, nil)
	if len(list.Body["items"].([]any)) != 1 {
		t.Fatal("history lost")
	}
	detail := alice.must(200, "GET", "/v1/match-runs/"+id, nil)
	if detail.Body["model"] != nil || detail.Body["cost_usd"] != "0.000000000000" {
		t.Fatalf("unexpected model/cost: %#v", detail.Body)
	}
	for _, p := range []string{path, "/v1/match-runs/" + id, "/v1/jobs/" + job + "/match-estimate"} {
		bob.must(404, "GET", p, nil)
	}
	bob.must(404, "POST", "/v1/match-runs/"+id+"/cancel", nil)
	hash := strings.Repeat("a", 64)
	bob.must(404, "PUT", "/v1/jobs/"+job+"/match-overrides/"+hash, map[string]any{"class": "ignore"})
	alice.must(422, "PUT", "/v1/jobs/"+job+"/match-overrides/"+hash, map[string]any{"class": "wrong"})
	alice.must(204, "PUT", "/v1/jobs/"+job+"/match-overrides/"+hash, map[string]any{"class": "ignore"})
	alice.must(204, "DELETE", "/v1/jobs/"+job+"/match-overrides/"+hash, nil)
	alice.must(204, "POST", "/v1/match-runs/"+id+"/cancel", nil)
	alice.must(204, "POST", "/v1/match-runs/"+id+"/cancel", nil)
	estimate := alice.must(200, "GET", fmt.Sprintf("/v1/jobs/%s/match-estimate", job), nil)
	if estimate.Body["available"] != false {
		t.Fatal("estimate guessed without quote")
	}
	for _, p := range []string{"/v1/admin/model-settings", "/v1/admin/decision-models"} {
		alice.must(403, "GET", p, nil)
	}
	alice.must(403, "PUT", "/v1/admin/model-settings", map[string]any{"decision_model": "test/model"}, "If-Match", "1")
}
func TestAutoMatchSaveIsAtomic(t *testing.T) {
	alice, _ := newServer(t)
	saved := alice.must(201, "POST", "/v1/jobs", map[string]any{"title": "Engineer", "description": "Go experience"})
	id := saved.Body["id"].(string)
	runs := alice.must(200, "GET", "/v1/jobs/"+id+"/match-runs", nil).Body["items"].([]any)
	if len(runs) != 1 || runs[0].(map[string]any)["trigger"] != "auto" {
		t.Fatal("save did not create automatic run")
	}
	board := alice.must(200, "GET", "/v1/jobs", nil)
	summary := board.Body["items"].([]any)[0].(map[string]any)["match"].(map[string]any)
	if summary["run_id"] != runs[0].(map[string]any)["id"] {
		t.Fatal("board run summary missing")
	}
}
