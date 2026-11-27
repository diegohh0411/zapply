package api

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/diegohh0411/zapply/internal/api/gen"
	"github.com/diegohh0411/zapply/internal/ats"
	"github.com/diegohh0411/zapply/internal/membership"
)

// ownerCall runs one owner-only platform function returning jsonb.
func (s *server) ownerCall(ctx context.Context, q string, args ...any) (json.RawMessage, error) {
	var raw json.RawMessage
	e := s.deps.Store.InTenant(ctx, uid(ctx), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, q, args...).Scan(&raw)
	})
	return raw, membership.Translate(e)
}

// GetJobSources lists the suppliers and board counts (owner only).
func (s *server) GetJobSources(ctx context.Context, _ gen.GetJobSourcesRequestObject) (gen.GetJobSourcesResponseObject, error) {
	return decodePlatform[gen.GetJobSources200JSONResponse](s.ownerCall(ctx, `SELECT platform.job_sources()`))
}

// PutSupplierInterval sets how often a supplier runs (owner only).
func (s *server) PutSupplierInterval(ctx context.Context, req gen.PutSupplierIntervalRequestObject) (gen.PutSupplierIntervalResponseObject, error) {
	return decodePlatform[gen.PutSupplierInterval200JSONResponse](s.ownerCall(ctx, `SELECT platform.save_supplier_interval($1,$2)`,
		req.Body.Name, int(req.Body.IntervalMinutes)))
}

// ListAtsBoards pages through the boards in one status (owner only).
func (s *server) ListAtsBoards(ctx context.Context, req gen.ListAtsBoardsRequestObject) (gen.ListAtsBoardsResponseObject, error) {
	limit, offset := 50, 0
	if req.Params.Limit != nil {
		limit = *req.Params.Limit
	}
	if req.Params.Offset != nil {
		offset = *req.Params.Offset
	}
	return decodePlatform[gen.ListAtsBoards200JSONResponse](s.ownerCall(ctx, `SELECT platform.ats_boards($1,$2,$3)`,
		string(req.Params.Status), limit, offset))
}

// maxPastedBoards bounds one paste; the active cap is 2,000 anyway.
const maxPastedBoards = 2000

// AddAtsBoards seeds boards from pasted links (owner only). Links that name
// no board are returned, not refused, so one typo doesn't lose the rest.
func (s *server) AddAtsBoards(ctx context.Context, req gen.AddAtsBoardsRequestObject) (gen.AddAtsBoardsResponseObject, error) {
	type board struct {
		ATS   string `json:"ats"`
		Board string `json:"board"`
	}
	boards := []board{}
	invalid := []string{}
	for _, link := range strings.FieldsFunc(req.Body.Links, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t' }) {
		system, slug, err := ats.Parse(link)
		if err != nil {
			invalid = append(invalid, link)
			continue
		}
		boards = append(boards, board{system, slug})
	}
	if len(boards) > maxPastedBoards {
		return nil, badRequest("paste at most 2000 boards at once")
	}
	out := gen.AddAtsBoards200JSONResponse{Invalid: invalid}
	if len(boards) == 0 {
		return out, nil
	}
	list, _ := json.Marshal(boards)
	raw, err := s.ownerCall(ctx, `SELECT platform.add_ats_boards($1)`, string(list))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	out.Invalid = invalid
	return out, nil
}

// PutAtsBoardStatus approves, rejects or disables a board (owner only).
func (s *server) PutAtsBoardStatus(ctx context.Context, req gen.PutAtsBoardStatusRequestObject) (gen.PutAtsBoardStatusResponseObject, error) {
	return decodePlatform[gen.PutAtsBoardStatus200JSONResponse](s.ownerCall(ctx, `SELECT platform.set_ats_board_status($1,$2,$3)`,
		string(req.Ats), req.Board, string(req.Body.Status)))
}

// PollAtsBoard reads an active board on the next tick (owner only).
func (s *server) PollAtsBoard(ctx context.Context, req gen.PollAtsBoardRequestObject) (gen.PollAtsBoardResponseObject, error) {
	return decodePlatform[gen.PollAtsBoard200JSONResponse](s.ownerCall(ctx, `SELECT platform.poll_ats_board($1,$2)`, string(req.Ats), req.Board))
}
