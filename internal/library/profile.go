package library

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/diegohh0411/zapply/internal/store/db"
)

type Profile struct {
	FullName  string
	Headline  string
	Email     string
	Phone     string
	Location  string
	Links     []Link
	Summary   string
	Version   int // 0 until first saved
	UpdatedAt time.Time
}

type Link struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

const maxLinks = 20

// GetProfile returns the profile, or an empty one with version 0.
func (s *Service) GetProfile(ctx context.Context, uid uuid.UUID) (Profile, error) {
	var p Profile
	err := s.tx(ctx, uid, func(q *db.Queries) error {
		row, err := q.GetProfile(ctx, uid)
		if errors.Is(err, pgx.ErrNoRows) {
			p = Profile{Links: []Link{}}
			return nil
		}
		if err != nil {
			return err
		}
		p, err = profileFromRow(row)
		return err
	})
	return p, err
}

// PutProfile replaces the profile if its version is still ifVersion (0 for
// a profile that was never saved).
func (s *Service) PutProfile(ctx context.Context, uid uuid.UUID, ifVersion int, in Profile) (Profile, error) {
	if err := cleanProfile(&in); err != nil {
		return Profile{}, err
	}
	links, _ := json.Marshal(in.Links)
	var out Profile
	err := s.tx(ctx, uid, func(q *db.Queries) error {
		var row db.Profile
		var err error
		if ifVersion == 0 {
			row, err = q.InsertProfile(ctx, db.InsertProfileParams{
				UserID: uid, FullName: in.FullName, Headline: in.Headline, Email: in.Email,
				Phone: in.Phone, Location: in.Location, Links: links, Summary: in.Summary,
			})
		} else {
			row, err = q.UpdateProfile(ctx, db.UpdateProfileParams{
				UserID: uid, Version: int32(ifVersion), FullName: in.FullName, Headline: in.Headline,
				Email: in.Email, Phone: in.Phone, Location: in.Location, Links: links, Summary: in.Summary,
			})
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrVersionMismatch
		}
		if err != nil {
			return err
		}
		if out, err = profileFromRow(row); err != nil {
			return err
		}
		return emit(ctx, q, uid, "profile.updated", uid, out.Version)
	})
	return out, err
}

func cleanProfile(p *Profile) error {
	var errs []FieldError
	add := func(f, m string) { errs = append(errs, FieldError{f, m}) }
	for _, f := range []struct {
		name string
		v    *string
		max  int
	}{
		{"full_name", &p.FullName, maxShortText}, {"headline", &p.Headline, maxShortText},
		{"email", &p.Email, maxShortText}, {"phone", &p.Phone, 50},
		{"location", &p.Location, maxShortText}, {"summary", &p.Summary, maxSummary},
	} {
		*f.v = strings.TrimSpace(*f.v)
		checkText(add, f.name, *f.v, f.max, false)
	}
	if p.Email != "" && (!strings.Contains(p.Email, "@") || strings.ContainsAny(p.Email, " <>")) {
		add("email", "must be an email address")
	}
	if p.Links == nil {
		p.Links = []Link{}
	}
	if len(p.Links) > maxLinks {
		add("links", "at most 20 links")
	}
	for i := range p.Links {
		l := &p.Links[i]
		l.Label, l.URL = strings.TrimSpace(l.Label), strings.TrimSpace(l.URL)
		checkText(add, "links["+itoa(i)+"].label", l.Label, 100, false)
		if u, err := url.Parse(l.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || len(l.URL) > maxURL {
			add("links["+itoa(i)+"].url", "must be an http(s) URL")
		}
	}
	if len(errs) > 0 {
		return &ValidationError{Fields: errs}
	}
	return nil
}

func profileFromRow(r db.Profile) (Profile, error) {
	p := Profile{
		FullName: r.FullName, Headline: r.Headline, Email: r.Email, Phone: r.Phone,
		Location: r.Location, Summary: r.Summary, Version: int(r.Version), UpdatedAt: r.UpdatedAt,
	}
	if err := json.Unmarshal(r.Links, &p.Links); err != nil {
		return p, err
	}
	if p.Links == nil {
		p.Links = []Link{}
	}
	return p, nil
}
