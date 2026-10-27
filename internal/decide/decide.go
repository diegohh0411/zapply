// Package decide caches independent typed questions by the exact content sent.
package decide

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/diegohh0411/zapply/internal/model/systemone"
	"github.com/diegohh0411/zapply/internal/store/db"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type Pending struct {
	Subject      string
	State        json.RawMessage
	Question     systemone.Question
	StateHash    string
	QuestionHash string
}

func New(subject string, state json.RawMessage, q systemone.Question) (Pending, error) {
	if e := systemone.ValidateQuestion(q); e != nil {
		return Pending{}, e
	}
	state, e := systemone.Canonical(state)
	if e != nil {
		return Pending{}, e
	}
	sh, e := systemone.Hash(state)
	if e != nil {
		return Pending{}, e
	}
	qh, e := systemone.Hash(q)
	return Pending{subject, state, q, sh, qh}, e
}
func Lookup(ctx context.Context, tx pgx.Tx, u uuid.UUID, model string, p Pending) (*systemone.Answer, error) {
	raw, e := db.New(tx).LookupDecisionAnswer(ctx, db.LookupDecisionAnswerParams{UserID: u, Model: model, StateHash: p.StateHash, QuestionHash: p.QuestionHash})
	if errors.Is(e, pgx.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var a systemone.Answer
	if e = json.Unmarshal(raw, &a); e != nil {
		return nil, e
	}
	if e = systemone.Validate(p.Question, a); e != nil {
		return nil, nil
	}
	return &a, nil
}

type Packed struct {
	Request systemone.Request
	Items   map[string]Pending
}

// Pack groups equal states deterministically. These are local storage budgets,
// not claims about provider context limits; QuoteDecision applies adapter limits.
func Pack(model string, pending []Pending, maxBytes, maxQuestions int) ([]Packed, error) {
	if maxBytes <= 0 || maxBytes > systemone.RequestBudget {
		maxBytes = systemone.RequestBudget
	}
	sort.SliceStable(pending, func(i, j int) bool {
		if pending[i].StateHash != pending[j].StateHash {
			return pending[i].StateHash < pending[j].StateHash
		}
		return pending[i].Subject < pending[j].Subject
	})
	result := []Packed{}
	var p Packed
	response := 0
	flush := func() {
		if len(p.Items) > 0 {
			result = append(result, p)
		}
		p = Packed{}
		response = 0
	}
	for _, item := range pending {
		if len(p.Items) > 0 && (string(p.Request.State) != string(item.State) || (maxQuestions > 0 && len(p.Items) >= maxQuestions)) {
			flush()
		}
		if p.Items == nil {
			p = Packed{systemone.Request{Model: model, State: item.State, Questions: map[string]systemone.Question{}}, map[string]Pending{}}
		}
		key := fmt.Sprintf("q%d", len(p.Items)+1)
		p.Request.Questions[key] = item.Question
		// Includes echoed legends and generous numeric/key overhead for each option.
		expected := 2048 + len(item.Question.Criteria)*2
		wire, _ := json.Marshal(p.Request)
		if len(wire) > maxBytes || response+expected > systemone.ResultBudget {
			delete(p.Request.Questions, key)
			if len(p.Items) == 0 {
				return nil, fmt.Errorf("question_too_large: %s", item.Subject)
			}
			flush()
			p = Packed{systemone.Request{Model: model, State: item.State, Questions: map[string]systemone.Question{"q1": item.Question}}, map[string]Pending{}}
			key = "q1"
			wire, _ = json.Marshal(p.Request)
			if len(wire) > maxBytes || expected > systemone.ResultBudget {
				return nil, fmt.Errorf("question_too_large: %s", item.Subject)
			}
		}
		p.Items[key] = item
		response += expected
	}
	flush()
	return result, nil
}

// Store writes the canonical request and correlation map in the operation's tx.
func Store(ctx context.Context, tx pgx.Tx, u, op uuid.UUID, p Packed) error {
	raw, e := systemone.Canonical(systemone.Raw(p.Request))
	if e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `INSERT INTO model_request(user_id,operation_id,body) VALUES($1,$2,$3)`, u, op, json.RawMessage(raw)); e != nil {
		return e
	}
	for key, item := range p.Items {
		if _, e = tx.Exec(ctx, `INSERT INTO decision_request_item(user_id,operation_id,request_key,model,state_hash,question_hash) VALUES($1,$2,$3,$4,$5,$6)`, u, op, key, p.Request.Model, item.StateHash, item.QuestionHash); e != nil {
			return e
		}
	}
	return nil
}

// Ingest returns only validated answers. Unexpected keys invalidate the envelope;
// other missing/invalid entries are discarded independently. Replays are safe.
func Ingest(ctx context.Context, tx pgx.Tx, u, op uuid.UUID, result string) (map[string]systemone.Answer, error) {
	valid := map[string]systemone.Answer{}
	var request systemone.Request
	var raw []byte
	if e := tx.QueryRow(ctx, `SELECT body FROM model_request WHERE user_id=$1 AND operation_id=$2`, u, op).Scan(&raw); e != nil {
		return nil, e
	}
	if e := json.Unmarshal(raw, &request); e != nil {
		return nil, e
	}
	var answers map[string]json.RawMessage
	if json.Unmarshal([]byte(result), &answers) != nil {
		return valid, nil
	}
	for key := range answers {
		if _, ok := request.Questions[key]; !ok {
			return valid, nil
		}
	}
	sh, e := systemone.Hash(request.State)
	if e != nil {
		return nil, e
	}
	for key, q := range request.Questions {
		var a systemone.Answer
		if json.Unmarshal(answers[key], &a) != nil || systemone.Validate(q, a) != nil {
			continue
		}
		qh, e := systemone.Hash(q)
		if e != nil {
			return nil, e
		}
		if _, e = tx.Exec(ctx, `INSERT INTO decision_answer(user_id,model,state_hash,question_hash,answer) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, u, request.Model, sh, qh, systemone.Raw(a)); e != nil {
			return nil, e
		}
		valid[key] = a
	}
	return valid, nil
}
