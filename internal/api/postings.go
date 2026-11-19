package api

import (
	"context"

	"github.com/diegohh0411/zapply/internal/api/gen"
)

// ReadPosting scrapes a posting's page for the new job dialog. Nothing is saved.
func (s *server) ReadPosting(ctx context.Context, req gen.ReadPostingRequestObject) (gen.ReadPostingResponseObject, error) {
	p, err := s.postings.Read(ctx, req.Body.Url)
	if err != nil {
		return nil, err
	}
	return gen.ReadPosting200JSONResponse{
		Title:          enumPtr[string](p.Title),
		Company:        enumPtr[string](p.Company),
		Location:       enumPtr[string](p.Location),
		WorkMode:       enumPtr[gen.WorkMode](p.WorkMode),
		EmploymentType: enumPtr[gen.EmploymentType](p.EmploymentType),
		SalaryMin:      p.SalaryMin,
		SalaryMax:      p.SalaryMax,
		SalaryCurrency: enumPtr[string](p.SalaryCurrency),
		SalaryPeriod:   enumPtr[gen.SalaryPeriod](p.SalaryPeriod),
		Description:    p.Description,
	}, nil
}
