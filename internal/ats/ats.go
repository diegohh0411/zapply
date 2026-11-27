// Package ats reads employers' public job boards on hosted applicant tracking
// systems (Greenhouse, Lever and Ashby). Each publishes every open posting of a
// board as JSON without credentials, keyed by the employer's board slug, and a
// posting appears there the moment it goes live.
package ats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Systems we read, as stored in platform.ats_board.ats.
const (
	Greenhouse = "greenhouse"
	Lever      = "lever"
	Ashby      = "ashby"
)

// Posting is one open job on a board.
type Posting struct {
	Key         string     `json:"key"`
	Title       string     `json:"title"`
	Location    string     `json:"location"`
	URL         string     `json:"url"`
	Remote      bool       `json:"remote"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	// Description is the posting's text, without markup.
	Description string `json:"description,omitempty"`
}

// Board is a board's postings and, when the ATS publishes it, the employer's name.
type Board struct {
	Company  string
	Postings []Posting
}

var (
	// ErrUnknownURL means the link is not a Greenhouse, Lever or Ashby board.
	ErrUnknownURL = errors.New("not a Greenhouse, Lever or Ashby job board link")
	// ErrNotFound means the ATS has no board with that slug.
	ErrNotFound = errors.New("job board not found")
)

var slugPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// ValidSlug reports whether s can name a board (and is safe in a URL path).
func ValidSlug(s string) bool { return slugPattern.MatchString(s) }

// Parse finds the ATS and board slug in a careers link, such as
// https://job-boards.greenhouse.io/acme, https://jobs.lever.co/acme/<id> or
// https://jobs.ashbyhq.com/acme. A posting's link names its board too.
func Parse(raw string) (system, board string, err error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" {
		return "", "", ErrUnknownURL
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	parts := strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })
	first := ""
	if len(parts) > 0 {
		first = parts[0]
	}
	switch host {
	case "boards.greenhouse.io", "job-boards.greenhouse.io", "boards.eu.greenhouse.io", "job-boards.eu.greenhouse.io":
		system, board = Greenhouse, first
		// Embedded boards: boards.greenhouse.io/embed/job_board?for=acme
		if first == "embed" {
			board = u.Query().Get("for")
		}
	case "boards-api.greenhouse.io":
		// boards-api.greenhouse.io/v1/boards/acme/jobs
		if len(parts) >= 3 && parts[0] == "v1" && parts[1] == "boards" {
			system, board = Greenhouse, parts[2]
		}
	case "jobs.lever.co":
		system, board = Lever, first
	case "jobs.ashbyhq.com":
		system, board = Ashby, first
	}
	if system == "" {
		return "", "", ErrUnknownURL
	}
	if !ValidSlug(board) {
		return "", "", fmt.Errorf("%w: the link names no board", ErrUnknownURL)
	}
	return system, board, nil
}

// BoardURL is the board's public careers page.
func BoardURL(system, board string) string {
	switch system {
	case Greenhouse:
		return "https://job-boards.greenhouse.io/" + board
	case Lever:
		return "https://jobs.lever.co/" + board
	case Ashby:
		return "https://jobs.ashbyhq.com/" + board
	}
	return ""
}

// Client reads boards. The zero value uses a 20-second timeout.
type Client struct {
	HTTP *http.Client
	// Base overrides each ATS's API origin, for tests.
	Base map[string]string
}

func (c *Client) base(system, def string) string {
	if b, ok := c.Base[system]; ok {
		return b
	}
	return def
}

// maxBody bounds one board's response; the largest boards are a few MB.
const maxBody = 32 << 20

// Fetch reads every open posting on a board.
func (c *Client) Fetch(ctx context.Context, system, board string) (Board, error) {
	if !ValidSlug(board) {
		return Board{}, ErrNotFound
	}
	switch system {
	case Greenhouse:
		return c.greenhouse(ctx, board)
	case Lever:
		return c.lever(ctx, board)
	case Ashby:
		return c.ashby(ctx, board)
	}
	return Board{}, fmt.Errorf("unknown ATS %q", system)
}

func (c *Client) get(ctx context.Context, u string, into any) error {
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "zapply-job-alerts/1.0")
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("board answered %s", res.Status)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxBody+1))
	if err != nil {
		return err
	}
	if len(body) > maxBody {
		return errors.New("board response too large")
	}
	return json.Unmarshal(body, into)
}

func parseTime(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return &t
}

func isRemote(s string) bool {
	return strings.Contains(strings.ToLower(s), "remote")
}

func (c *Client) greenhouse(ctx context.Context, board string) (Board, error) {
	base := c.base(Greenhouse, "https://boards-api.greenhouse.io")
	var jobs struct {
		Jobs []struct {
			ID             int64  `json:"id"`
			Title          string `json:"title"`
			AbsoluteURL    string `json:"absolute_url"`
			FirstPublished string `json:"first_published"`
			UpdatedAt      string `json:"updated_at"`
			Content        string `json:"content"`
			Location       struct {
				Name string `json:"name"`
			} `json:"location"`
		} `json:"jobs"`
	}
	if err := c.get(ctx, base+"/v1/boards/"+url.PathEscape(board)+"/jobs?content=true", &jobs); err != nil {
		return Board{}, err
	}
	var meta struct {
		Name string `json:"name"`
	}
	// The name is a nicety: a board without it still works.
	_ = c.get(ctx, base+"/v1/boards/"+url.PathEscape(board), &meta)
	out := Board{Company: meta.Name, Postings: make([]Posting, 0, len(jobs.Jobs))}
	for _, j := range jobs.Jobs {
		published := parseTime(j.FirstPublished)
		if published == nil {
			published = parseTime(j.UpdatedAt)
		}
		out.Postings = append(out.Postings, Posting{Key: fmt.Sprint(j.ID), Title: j.Title, Location: j.Location.Name,
			URL: j.AbsoluteURL, Remote: isRemote(j.Location.Name), PublishedAt: published, Description: htmlText(j.Content)})
	}
	return out, nil
}

func (c *Client) lever(ctx context.Context, board string) (Board, error) {
	base := c.base(Lever, "https://api.lever.co")
	var jobs []struct {
		ID            string `json:"id"`
		Text          string `json:"text"`
		HostedURL     string `json:"hostedUrl"`
		CreatedAt     int64  `json:"createdAt"`
		WorkplaceType string `json:"workplaceType"`
		Description   string `json:"descriptionPlain"`
		Additional    string `json:"additionalPlain"`
		Categories    struct {
			Location     string   `json:"location"`
			AllLocations []string `json:"allLocations"`
		} `json:"categories"`
	}
	if err := c.get(ctx, base+"/v0/postings/"+url.PathEscape(board)+"?mode=json", &jobs); err != nil {
		return Board{}, err
	}
	out := Board{Postings: make([]Posting, 0, len(jobs))}
	for _, j := range jobs {
		loc := j.Categories.Location
		if len(j.Categories.AllLocations) > 1 {
			loc = strings.Join(j.Categories.AllLocations, "; ")
		}
		var published *time.Time
		if j.CreatedAt > 0 {
			t := time.UnixMilli(j.CreatedAt).UTC()
			published = &t
		}
		out.Postings = append(out.Postings, Posting{Key: j.ID, Title: j.Text, Location: loc, URL: j.HostedURL,
			Remote: j.WorkplaceType == "remote" || isRemote(loc), PublishedAt: published,
			Description: strings.TrimSpace(j.Description + "\n\n" + j.Additional)})
	}
	return out, nil
}

func (c *Client) ashby(ctx context.Context, board string) (Board, error) {
	base := c.base(Ashby, "https://api.ashbyhq.com")
	var jobs struct {
		Jobs []struct {
			ID                 string `json:"id"`
			Title              string `json:"title"`
			Location           string `json:"location"`
			IsRemote           bool   `json:"isRemote"`
			WorkplaceType      string `json:"workplaceType"`
			JobURL             string `json:"jobUrl"`
			PublishedAt        string `json:"publishedAt"`
			IsListed           *bool  `json:"isListed"`
			Description        string `json:"descriptionPlain"`
			SecondaryLocations []struct {
				Location string `json:"location"`
			} `json:"secondaryLocations"`
		} `json:"jobs"`
	}
	if err := c.get(ctx, base+"/posting-api/job-board/"+url.PathEscape(board), &jobs); err != nil {
		return Board{}, err
	}
	out := Board{Postings: make([]Posting, 0, len(jobs.Jobs))}
	for _, j := range jobs.Jobs {
		if j.IsListed != nil && !*j.IsListed {
			continue
		}
		locs := []string{j.Location}
		for _, s := range j.SecondaryLocations {
			locs = append(locs, s.Location)
		}
		loc := strings.Join(locs, "; ")
		out.Postings = append(out.Postings, Posting{Key: j.ID, Title: j.Title, Location: loc, URL: j.JobURL,
			Remote: j.IsRemote || strings.EqualFold(j.WorkplaceType, "remote") || isRemote(loc), PublishedAt: parseTime(j.PublishedAt),
			Description: strings.TrimSpace(j.Description)})
	}
	return out, nil
}

// CompanyFromSlug guesses a display name when the ATS publishes none.
func CompanyFromSlug(board string) string {
	words := strings.FieldsFunc(board, func(r rune) bool { return r == '-' || r == '_' || r == '.' })
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	if len(words) == 0 {
		return board
	}
	return strings.Join(words, " ")
}
