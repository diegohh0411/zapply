// Package alerts tells members about new openings on the job boards they
// watch, within PollInterval of publication, so they can apply among the first.
//
// A board watch names an employer's board on a hosted ATS (see package ats)
// plus optional filters. The worker polls each watched board once, however
// many members watch it, and the database decides what is new and who it
// matches (migration 00037).
//
// A search watch is a saved query run across every company through a job
// search provider (package jobsearch) at the member's chosen interval. Its
// polls cost provider credits, so it is an entitlement of the member's plan
// and every poll is a charge on their ledger (migration 00040).
//
// A posting alerts once per member, however many watches find it: each alert
// records the posting's identities in job_alert_key (see keys.go).
package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/diegohh0411/zapply/internal/ats"
	"github.com/diegohh0411/zapply/internal/catalog"
	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PollInterval is how often each watched board is read.
const PollInterval = 30 * time.Minute

// Fetcher reads a board; *ats.Client is the real one.
type Fetcher interface {
	Fetch(ctx context.Context, system, board string) (ats.Board, error)
}

type Service struct {
	store   *store.Store
	ats     Fetcher
	search  Searcher
	keys    KeyOpener
	catalog *catalog.Catalog
}

func New(st *store.Store, f Fetcher) *Service {
	if f == nil {
		f = &ats.Client{}
	}
	return &Service{store: st, ats: f}
}

// Watch kinds.
const (
	KindBoard  = "board"
	KindSearch = "search"
)

// Watch is one board or search a member follows.
type Watch struct {
	ID            uuid.UUID  `json:"id"`
	Kind          string     `json:"kind"`
	ATS           *string    `json:"ats"`
	Board         *string    `json:"board"`
	BoardURL      *string    `json:"board_url"`
	Company       string     `json:"company"`
	Keywords      []string   `json:"keywords"`
	Locations     []string   `json:"locations"`
	Country       *string    `json:"country"`
	RemoteOnly    bool       `json:"remote_only"`
	EveryHours    *int       `json:"every_hours"`
	Paused        bool       `json:"paused"`
	LastCheckedAt *time.Time `json:"last_checked_at"`
	LastError     string     `json:"last_error"`
	BlockedReason string     `json:"blocked_reason"`
	NextCheckAt   *time.Time `json:"next_check_at"`
	Spent30dUSD   string     `json:"spent_30d_usd"`
	CreatedAt     time.Time  `json:"created_at"`
}

// Allowance is what the member's plan allows for search watchers.
type Allowance struct {
	Allowed    int     `json:"allowed"`
	Used       int     `json:"used"`
	Available  bool    `json:"available"`
	CreditUSD  *string `json:"credit_usd"`
	MaxResults int     `json:"max_results"`
}

// Alert is one new posting that matched a watch.
type Alert struct {
	ID          uuid.UUID  `json:"id"`
	WatchID     uuid.UUID  `json:"watch_id"`
	Company     string     `json:"company"`
	Title       string     `json:"title"`
	Location    string     `json:"location"`
	URL         string     `json:"url"`
	Remote      bool       `json:"remote"`
	PublishedAt *time.Time `json:"published_at"`
	FoundAt     time.Time  `json:"found_at"`
}

// Input creates a watch (a board's URL, or a search's terms) or changes one
// (nil fields stay as they are).
type Input struct {
	Kind       string
	URL        string
	Company    *string
	Keywords   *[]string
	Locations  *[]string
	Country    *string
	EveryHours *int
	RemoteOnly *bool
	Paused     *bool
}

// DefaultEveryHours is how often a new search runs unless the member chooses.
const DefaultEveryHours = 2

func everyHours(n int) error {
	switch n {
	case 1, 2, 6, 24:
		return nil
	}
	return invalid("every_hours", "choose every 1, 2, 6 or 24 hours")
}

// country validates an ISO country code; "" is none.
func country(s string) (*string, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" {
		return nil, nil
	}
	if len(s) != 2 || s[0] < 'A' || s[0] > 'Z' || s[1] < 'A' || s[1] > 'Z' {
		return nil, invalid("country", "use a two-letter country code, such as MX")
	}
	return &s, nil
}

// MaxWatches bounds one member's watches.
const MaxWatches = 100

func invalid(field, msg string) error {
	return &library.ValidationError{Fields: []library.FieldError{{Field: field, Message: msg}}}
}

// terms trims, drops blanks and duplicates, and bounds a filter list.
func terms(field string, in []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, t := range in {
		t = strings.Join(strings.Fields(t), " ")
		if t == "" || seen[strings.ToLower(t)] {
			continue
		}
		if utf8.RuneCountInString(t) > 100 {
			return nil, invalid(field, "each term is at most 100 characters")
		}
		seen[strings.ToLower(t)] = true
		out = append(out, t)
	}
	if len(out) > 20 {
		return nil, invalid(field, "at most 20 terms")
	}
	return out, nil
}

func company(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || utf8.RuneCountInString(s) > 200 {
		return "", invalid("company", "must be 1 to 200 characters")
	}
	return s, nil
}

const watchColumns = `id,kind,ats,board,company,keywords,locations,country,remote_only,every_hours,paused,created_at`

func scanWatch(row pgx.Row) (Watch, error) {
	w := Watch{Spent30dUSD: "0"}
	err := row.Scan(&w.ID, &w.Kind, &w.ATS, &w.Board, &w.Company, &w.Keywords, &w.Locations, &w.Country, &w.RemoteOnly, &w.EveryHours, &w.Paused, &w.CreatedAt)
	if w.ATS != nil && w.Board != nil {
		u := ats.BoardURL(*w.ATS, *w.Board)
		w.BoardURL = &u
	}
	return w, err
}

type boardHealth struct {
	ATS       string     `json:"ats"`
	Board     string     `json:"board"`
	LastOKAt  *time.Time `json:"last_ok_at"`
	LastError string     `json:"last_error"`
}

type searchHealth struct {
	WatchID       uuid.UUID  `json:"watch_id"`
	LastOKAt      *time.Time `json:"last_ok_at"`
	LastError     string     `json:"last_error"`
	BlockedReason string     `json:"blocked_reason"`
	NextPollAt    *time.Time `json:"next_poll_at"`
	Spent30dUSD   string     `json:"spent_30d_usd"`
}

type searchState struct {
	Allowance
	Watches []searchHealth `json:"watches"`
}

func loadSearchState(ctx context.Context, tx pgx.Tx) (searchState, error) {
	var raw []byte
	var st searchState
	if err := tx.QueryRow(ctx, `SELECT platform.my_search_watches()`).Scan(&raw); err != nil {
		return st, err
	}
	err := json.Unmarshal(raw, &st)
	return st, err
}

// withHealth adds each board's last poll, and each search's state, to the
// watches. It returns the member's search allowance.
func withHealth(ctx context.Context, tx pgx.Tx, ws []Watch) (Allowance, error) {
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT platform.my_ats_boards()`).Scan(&raw); err != nil {
		return Allowance{}, err
	}
	var hs []boardHealth
	if err := json.Unmarshal(raw, &hs); err != nil {
		return Allowance{}, err
	}
	by := map[string]boardHealth{}
	for _, h := range hs {
		by[h.ATS+"/"+h.Board] = h
	}
	st, err := loadSearchState(ctx, tx)
	if err != nil {
		return Allowance{}, err
	}
	searches := map[uuid.UUID]searchHealth{}
	for _, h := range st.Watches {
		searches[h.WatchID] = h
	}
	for i := range ws {
		w := &ws[i]
		if w.Kind == KindSearch {
			if h, ok := searches[w.ID]; ok {
				w.LastCheckedAt, w.LastError, w.BlockedReason, w.Spent30dUSD = h.LastOKAt, h.LastError, h.BlockedReason, h.Spent30dUSD
				if !w.Paused {
					w.NextCheckAt = h.NextPollAt
				}
			}
			continue
		}
		if w.ATS == nil || w.Board == nil {
			continue
		}
		if h, ok := by[*w.ATS+"/"+*w.Board]; ok {
			w.LastCheckedAt, w.LastError = h.LastOKAt, h.LastError
		}
	}
	return st.Allowance, nil
}

// Watches lists the member's watches, newest first, and their search allowance.
func (s *Service) Watches(ctx context.Context, u uuid.UUID) ([]Watch, Allowance, error) {
	out := []Watch{}
	var allowance Allowance
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+watchColumns+` FROM job_watch WHERE user_id=$1 ORDER BY created_at DESC,id DESC`, u)
		if err != nil {
			return err
		}
		for rows.Next() {
			w, err := scanWatch(rows)
			if err != nil {
				rows.Close()
				return err
			}
			out = append(out, w)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		allowance, err = withHealth(ctx, tx, out)
		return err
	})
	return out, allowance, err
}

func (s *Service) get(ctx context.Context, tx pgx.Tx, u, id uuid.UUID) (Watch, error) {
	w, err := scanWatch(tx.QueryRow(ctx, `SELECT `+watchColumns+` FROM job_watch WHERE user_id=$1 AND id=$2`, u, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return w, library.ErrNotFound
	}
	if err != nil {
		return w, err
	}
	ws := []Watch{w}
	_, err = withHealth(ctx, tx, ws)
	return ws[0], err
}

// Create starts a board or search watch.
func (s *Service) Create(ctx context.Context, u uuid.UUID, in Input) (Watch, error) {
	switch in.Kind {
	case "", KindBoard:
		return s.createBoard(ctx, u, in)
	case KindSearch:
		return s.createSearch(ctx, u, in)
	}
	return Watch{}, invalid("kind", "must be board or search")
}

// createBoard starts watching the board a careers link names. It reads the
// board once, so a mistyped slug fails here rather than silently never alerting.
func (s *Service) createBoard(ctx context.Context, u uuid.UUID, in Input) (Watch, error) {
	if in.Country != nil || in.EveryHours != nil {
		return Watch{}, invalid("kind", "a country and an interval are for searches; boards are read every 30 minutes")
	}
	system, board, err := ats.Parse(in.URL)
	if err != nil {
		return Watch{}, invalid("url", "paste a Greenhouse, Lever or Ashby careers link, like jobs.lever.co/acme")
	}
	keywords, err := terms("keywords", deref(in.Keywords))
	if err != nil {
		return Watch{}, err
	}
	locations, err := terms("locations", deref(in.Locations))
	if err != nil {
		return Watch{}, err
	}
	var name string
	if in.Company != nil && strings.TrimSpace(*in.Company) != "" {
		if name, err = company(*in.Company); err != nil {
			return Watch{}, err
		}
	}
	read, err := s.ats.Fetch(ctx, system, board)
	switch {
	case errors.Is(err, ats.ErrNotFound):
		return Watch{}, invalid("url", "that job board doesn't exist; check the link")
	case err != nil:
		return Watch{}, invalid("url", "couldn't read that job board right now; try again")
	}
	if name == "" {
		name = strings.TrimSpace(read.Company)
		if name == "" || utf8.RuneCountInString(name) > 200 {
			name = ats.CompanyFromSlug(board)
		}
	}
	var w Watch
	err = s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM job_watch WHERE user_id=$1 AND kind='board'`, u).Scan(&n); err != nil {
			return err
		}
		if n >= MaxWatches {
			return invalid("url", "you can watch at most 100 job boards")
		}
		id := uuid.Must(uuid.NewV7())
		_, err := tx.Exec(ctx, `INSERT INTO job_watch(id,user_id,ats,board,company,keywords,locations,remote_only) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
			id, u, system, board, name, keywords, locations, in.RemoteOnly != nil && *in.RemoteOnly)
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			return &library.ConflictError{Message: "you already watch this job board"}
		}
		if err != nil {
			return err
		}
		w, err = s.get(ctx, tx, u, id)
		return err
	})
	return w, err
}

// createSearch saves a search, if the member's plan has room for it. Its
// first poll comes one interval later and announces only what is new by then.
func (s *Service) createSearch(ctx context.Context, u uuid.UUID, in Input) (Watch, error) {
	if in.URL != "" {
		return Watch{}, invalid("url", "a search has no careers link")
	}
	keywords, err := terms("keywords", deref(in.Keywords))
	if err != nil {
		return Watch{}, err
	}
	if len(keywords) == 0 {
		return Watch{}, invalid("keywords", "name at least one title to search for, such as software engineer")
	}
	for _, k := range keywords {
		if strings.Contains(k, ",") {
			return Watch{}, invalid("keywords", "separate titles with commas")
		}
	}
	locations, err := terms("locations", deref(in.Locations))
	if err != nil {
		return Watch{}, err
	}
	var code *string
	if in.Country != nil {
		if code, err = country(*in.Country); err != nil {
			return Watch{}, err
		}
	}
	every := DefaultEveryHours
	if in.EveryHours != nil {
		every = *in.EveryHours
	}
	if err := everyHours(every); err != nil {
		return Watch{}, err
	}
	name := ""
	if in.Company != nil && strings.TrimSpace(*in.Company) != "" {
		if name, err = company(*in.Company); err != nil {
			return Watch{}, err
		}
	} else {
		name = searchName(keywords, locations, code)
	}
	remote := in.RemoteOnly != nil && *in.RemoteOnly
	var w Watch
	err = s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		st, err := loadSearchState(ctx, tx)
		if err != nil {
			return err
		}
		switch {
		case !st.Available:
			return invalid("kind", "search watchers aren't set up on this server yet")
		case st.Allowed == 0:
			return invalid("kind", "your plan doesn't include search watchers")
		case st.Used >= st.Allowed:
			return invalid("kind", fmt.Sprintf("your plan allows %d search watchers; remove one first", st.Allowed))
		}
		id := uuid.Must(uuid.NewV7())
		_, err = tx.Exec(ctx, `INSERT INTO job_watch(id,user_id,kind,company,keywords,locations,country,remote_only,every_hours) VALUES($1,$2,'search',$3,$4,$5,$6,$7,$8)`,
			id, u, name, keywords, locations, code, remote, every)
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "ZA403" {
			return invalid("kind", "your plan has no room for another search watcher")
		}
		if err != nil {
			return err
		}
		w, err = s.get(ctx, tx, u, id)
		return err
	})
	return w, err
}

// searchName names a search after what it looks for: "Software engineer ·
// Monterrey · MX".
func searchName(keywords, locations []string, code *string) string {
	parts := []string{strings.Join(keywords, ", ")}
	if len(locations) > 0 {
		parts = append(parts, strings.Join(locations, ", "))
	}
	if code != nil {
		parts = append(parts, *code)
	}
	name := strings.Join(parts, " · ")
	if utf8.RuneCountInString(name) > 200 {
		name = string([]rune(name)[:199]) + "…"
	}
	return name
}

// Update changes a watch's name, filters, interval or pause.
func (s *Service) Update(ctx context.Context, u, id uuid.UUID, in Input) (Watch, error) {
	sets, args := []string{}, []any{u, id}
	add := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, col+"=$"+strconv.Itoa(len(args)))
	}
	if in.Company != nil {
		name, err := company(*in.Company)
		if err != nil {
			return Watch{}, err
		}
		add("company", name)
	}
	clearsKeywords := false
	if in.Keywords != nil {
		k, err := terms("keywords", *in.Keywords)
		if err != nil {
			return Watch{}, err
		}
		clearsKeywords = len(k) == 0
		add("keywords", k)
	}
	if in.Locations != nil {
		l, err := terms("locations", *in.Locations)
		if err != nil {
			return Watch{}, err
		}
		add("locations", l)
	}
	if in.RemoteOnly != nil {
		add("remote_only", *in.RemoteOnly)
	}
	if in.Paused != nil {
		add("paused", *in.Paused)
	}
	searchOnly := in.Country != nil || in.EveryHours != nil
	if in.Country != nil {
		code, err := country(*in.Country)
		if err != nil {
			return Watch{}, err
		}
		add("country", code)
	}
	if in.EveryHours != nil {
		if err := everyHours(*in.EveryHours); err != nil {
			return Watch{}, err
		}
		add("every_hours", *in.EveryHours)
	}
	var w Watch
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		var kind string
		if err := tx.QueryRow(ctx, `SELECT kind FROM job_watch WHERE user_id=$1 AND id=$2`, u, id).Scan(&kind); errors.Is(err, pgx.ErrNoRows) {
			return library.ErrNotFound
		} else if err != nil {
			return err
		}
		if kind == KindBoard && searchOnly {
			return invalid("kind", "a country and an interval are for searches; boards are read every 30 minutes")
		}
		if kind == KindSearch && clearsKeywords {
			return invalid("keywords", "a search needs at least one title")
		}
		if len(sets) > 0 {
			tag, err := tx.Exec(ctx, `UPDATE job_watch SET `+strings.Join(sets, ",")+`,version=version+1,updated_at=now() WHERE user_id=$1 AND id=$2`, args...)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return library.ErrNotFound
			}
		}
		var err error
		w, err = s.get(ctx, tx, u, id)
		return err
	})
	return w, err
}

// Delete stops watching a board. Alerts it made go with it.
func (s *Service) Delete(ctx context.Context, u, id uuid.UUID) error {
	return s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM job_watch WHERE user_id=$1 AND id=$2`, u, id)
		if err == nil && tag.RowsAffected() == 0 {
			err = library.ErrNotFound
		}
		return err
	})
}

// AlertsByID loads alerts for notifications, inside the caller's tenant transaction.
func AlertsByID(ctx context.Context, tx pgx.Tx, u uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]Alert, error) {
	out := map[uuid.UUID]Alert{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `SELECT id,watch_id,company,title,location,url,remote,published_at,found_at FROM job_alert WHERE user_id=$1 AND id=ANY($2)`, u, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a Alert
		if err := rows.Scan(&a.ID, &a.WatchID, &a.Company, &a.Title, &a.Location, &a.URL, &a.Remote, &a.PublishedAt, &a.FoundAt); err != nil {
			return nil, err
		}
		out[a.ID] = a
	}
	return out, rows.Err()
}

func deref(p *[]string) []string {
	if p == nil {
		return nil
	}
	return *p
}
