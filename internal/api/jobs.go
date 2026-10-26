package api

import (
	"context"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/diegohh0411/zapply/internal/api/gen"
	"github.com/diegohh0411/zapply/internal/jobs"
)

// Statuses

func (s *server) ListJobStatuses(ctx context.Context, _ gen.ListJobStatusesRequestObject) (gen.ListJobStatusesResponseObject, error) {
	sts, err := s.jobs.ListStatuses(ctx, uid(ctx))
	if err != nil {
		return nil, err
	}
	out := make(gen.ListJobStatuses200JSONResponse, len(sts))
	for i, st := range sts {
		out[i] = toJobStatus(st)
	}
	return out, nil
}

func (s *server) CreateJobStatus(ctx context.Context, req gen.CreateJobStatusRequestObject) (gen.CreateJobStatusResponseObject, error) {
	st, err := s.jobs.CreateStatus(ctx, uid(ctx), statusInput(*req.Body))
	if err != nil {
		return nil, err
	}
	return gen.CreateJobStatus201JSONResponse(toJobStatus(st)), nil
}

func (s *server) UpdateJobStatus(ctx context.Context, req gen.UpdateJobStatusRequestObject) (gen.UpdateJobStatusResponseObject, error) {
	st, err := s.jobs.UpdateStatus(ctx, uid(ctx), req.Id, statusInput(*req.Body))
	if err != nil {
		return nil, err
	}
	return gen.UpdateJobStatus200JSONResponse(toJobStatus(st)), nil
}

func (s *server) MoveJobStatus(ctx context.Context, req gen.MoveJobStatusRequestObject) (gen.MoveJobStatusResponseObject, error) {
	st, err := s.jobs.MoveStatus(ctx, uid(ctx), req.Id, jobs.MoveInput{AfterID: req.Body.AfterId, BeforeID: req.Body.BeforeId})
	if err != nil {
		return nil, err
	}
	return gen.MoveJobStatus200JSONResponse(toJobStatus(st)), nil
}

func (s *server) DeleteJobStatus(ctx context.Context, req gen.DeleteJobStatusRequestObject) (gen.DeleteJobStatusResponseObject, error) {
	if err := s.jobs.DeleteStatus(ctx, uid(ctx), req.Id, req.Params.MoveTo); err != nil {
		return nil, err
	}
	return gen.DeleteJobStatus204Response{}, nil
}

func statusInput(b gen.JobStatusInput) jobs.StatusInput {
	in := jobs.StatusInput{Name: b.Name, Stage: jobs.Stage(b.Stage)}
	if b.Color != nil {
		in.Color = string(*b.Color)
	}
	return in
}

func toJobStatus(st jobs.Status) gen.JobStatus {
	return gen.JobStatus{
		Id: st.ID, Name: st.Name, Color: gen.JobStatusColor(st.Color), Stage: gen.JobStage(st.Stage),
		CreatedAt: st.CreatedAt, UpdatedAt: st.UpdatedAt,
	}
}

// Jobs

func (s *server) ListJobs(ctx context.Context, req gen.ListJobsRequestObject) (gen.ListJobsResponseObject, error) {
	p := req.Params
	f := jobs.ListFilter{
		StatusIDs: deref(p.StatusId), Q: deref(p.Q), IncludeArchived: deref(p.IncludeArchived),
		Cursor: deref(p.Cursor), Limit: deref(p.Limit),
	}
	for _, st := range deref(p.Stage) {
		f.Stages = append(f.Stages, string(st))
	}
	if p.Sort != nil {
		f.Sort = jobs.Sort(*p.Sort)
	}
	list, next, err := s.jobs.ListJobs(ctx, uid(ctx), f)
	if err != nil {
		return nil, err
	}
	out := gen.ListJobs200JSONResponse{Items: make([]gen.JobSummary, len(list))}
	for i, j := range list {
		out.Items[i] = toJobSummary(j)
	}
	if next != "" {
		out.NextCursor = &next
	}
	return out, nil
}

func (s *server) GetJob(ctx context.Context, req gen.GetJobRequestObject) (gen.GetJobResponseObject, error) {
	j, err := s.jobs.GetJob(ctx, uid(ctx), req.Id)
	if err != nil {
		return nil, err
	}
	return gen.GetJob200JSONResponse{Body: toJob(j), Headers: gen.GetJob200ResponseHeaders{ETag: etag(j.Version)}}, nil
}

func (s *server) CreateJob(ctx context.Context, req gen.CreateJobRequestObject) (gen.CreateJobResponseObject, error) {
	j, err := s.jobs.CreateJob(ctx, uid(ctx), jobInput(*req.Body))
	if err != nil {
		return nil, err
	}
	return gen.CreateJob201JSONResponse{Body: toJob(j), Headers: gen.CreateJob201ResponseHeaders{ETag: etag(j.Version)}}, nil
}

func (s *server) PutJob(ctx context.Context, req gen.PutJobRequestObject) (gen.PutJobResponseObject, error) {
	ver, err := parseIfMatch(req.Params.IfMatch)
	if err != nil {
		return nil, err
	}
	j, err := s.jobs.UpdateJob(ctx, uid(ctx), req.Id, ver, jobInput(*req.Body))
	if err != nil {
		return nil, err
	}
	return gen.PutJob200JSONResponse{Body: toJob(j), Headers: gen.PutJob200ResponseHeaders{ETag: etag(j.Version)}}, nil
}

func (s *server) MoveJob(ctx context.Context, req gen.MoveJobRequestObject) (gen.MoveJobResponseObject, error) {
	b := req.Body
	j, err := s.jobs.MoveJob(ctx, uid(ctx), req.Id, jobs.JobMove{
		StatusID: b.StatusId, MoveInput: jobs.MoveInput{AfterID: b.AfterId, BeforeID: b.BeforeId},
	})
	if err != nil {
		return nil, err
	}
	return gen.MoveJob200JSONResponse{Body: toJob(j), Headers: gen.MoveJob200ResponseHeaders{ETag: etag(j.Version)}}, nil
}

func (s *server) ArchiveJob(ctx context.Context, req gen.ArchiveJobRequestObject) (gen.ArchiveJobResponseObject, error) {
	j, err := s.jobs.SetJobArchived(ctx, uid(ctx), req.Id, true)
	if err != nil {
		return nil, err
	}
	return gen.ArchiveJob200JSONResponse{Body: toJob(j), Headers: gen.ArchiveJob200ResponseHeaders{ETag: etag(j.Version)}}, nil
}

func (s *server) UnarchiveJob(ctx context.Context, req gen.UnarchiveJobRequestObject) (gen.UnarchiveJobResponseObject, error) {
	j, err := s.jobs.SetJobArchived(ctx, uid(ctx), req.Id, false)
	if err != nil {
		return nil, err
	}
	return gen.UnarchiveJob200JSONResponse{Body: toJob(j), Headers: gen.UnarchiveJob200ResponseHeaders{ETag: etag(j.Version)}}, nil
}

func (s *server) DeleteJob(ctx context.Context, req gen.DeleteJobRequestObject) (gen.DeleteJobResponseObject, error) {
	if err := s.jobs.DeleteJob(ctx, uid(ctx), req.Id); err != nil {
		return nil, err
	}
	return gen.DeleteJob204Response{}, nil
}

func jobInput(b gen.JobInput) jobs.JobInput {
	in := jobs.JobInput{
		StatusID: b.StatusId, Title: b.Title, Company: deref(b.Company), URL: deref(b.Url),
		Location: deref(b.Location), WorkMode: string(deref(b.WorkMode)),
		EmploymentType: string(deref(b.EmploymentType)), SalaryMin: b.SalaryMin, SalaryMax: b.SalaryMax,
		SalaryCurrency: deref(b.SalaryCurrency), SalaryPeriod: string(deref(b.SalaryPeriod)),
		Source: deref(b.Source), Description: deref(b.Description), Notes: deref(b.Notes),
	}
	if b.AppliedOn != nil {
		t := b.AppliedOn.Time
		in.AppliedOn = &t
	}
	return in
}

func toJobSummary(j jobs.Job) gen.JobSummary {
	out := gen.JobSummary{
		Id: j.ID, StatusId: j.StatusID, Title: j.Title, Company: j.Company, Url: j.URL, Location: j.Location,
		WorkMode: enumPtr[gen.WorkMode](j.WorkMode), EmploymentType: enumPtr[gen.EmploymentType](j.EmploymentType),
		SalaryMin: j.SalaryMin, SalaryMax: j.SalaryMax, SalaryCurrency: j.SalaryCurrency,
		SalaryPeriod: enumPtr[gen.SalaryPeriod](j.SalaryPeriod), Source: j.Source, Position: j.Position,
		Version: j.Version, Archived: j.ArchivedAt != nil, StatusChangedAt: j.StatusChangedAt,
		CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt,
	}
	if j.AppliedOn != nil {
		out.AppliedOn = &openapi_types.Date{Time: *j.AppliedOn}
	}
	return out
}

func toJob(j jobs.Job) gen.Job {
	s := toJobSummary(j)
	return gen.Job{
		Id: s.Id, StatusId: s.StatusId, Title: s.Title, Company: s.Company, Url: s.Url, Location: s.Location,
		WorkMode: s.WorkMode, EmploymentType: s.EmploymentType, SalaryMin: s.SalaryMin, SalaryMax: s.SalaryMax,
		SalaryCurrency: s.SalaryCurrency, SalaryPeriod: s.SalaryPeriod, Source: s.Source, AppliedOn: s.AppliedOn,
		Position: s.Position, Version: s.Version, Archived: s.Archived, StatusChangedAt: s.StatusChangedAt,
		CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt, Description: j.Description, Notes: j.Notes,
	}
}

func enumPtr[T ~string](v string) *T {
	if v == "" {
		return nil
	}
	t := T(v)
	return &t
}

// Activity

func (s *server) ListJobActivities(ctx context.Context, req gen.ListJobActivitiesRequestObject) (gen.ListJobActivitiesResponseObject, error) {
	acts, err := s.jobs.ListActivities(ctx, uid(ctx), req.Id)
	if err != nil {
		return nil, err
	}
	out := make(gen.ListJobActivities200JSONResponse, len(acts))
	for i, a := range acts {
		out[i] = toJobActivity(a)
	}
	return out, nil
}

func (s *server) CreateJobNote(ctx context.Context, req gen.CreateJobNoteRequestObject) (gen.CreateJobNoteResponseObject, error) {
	a, err := s.jobs.AddNote(ctx, uid(ctx), req.Id, jobs.NoteInput{Body: req.Body.Body, OccurredAt: req.Body.OccurredAt})
	if err != nil {
		return nil, err
	}
	return gen.CreateJobNote201JSONResponse(toJobActivity(a)), nil
}

func (s *server) UpdateJobNote(ctx context.Context, req gen.UpdateJobNoteRequestObject) (gen.UpdateJobNoteResponseObject, error) {
	a, err := s.jobs.UpdateNote(ctx, uid(ctx), req.Id, req.ActivityId, jobs.NoteInput{Body: req.Body.Body, OccurredAt: req.Body.OccurredAt})
	if err != nil {
		return nil, err
	}
	return gen.UpdateJobNote200JSONResponse(toJobActivity(a)), nil
}

func (s *server) DeleteJobNote(ctx context.Context, req gen.DeleteJobNoteRequestObject) (gen.DeleteJobNoteResponseObject, error) {
	if err := s.jobs.DeleteNote(ctx, uid(ctx), req.Id, req.ActivityId); err != nil {
		return nil, err
	}
	return gen.DeleteJobNote204Response{}, nil
}

func toJobActivity(a jobs.Activity) gen.JobActivity {
	return gen.JobActivity{
		Id: a.ID, JobId: a.JobID, Type: gen.JobActivityType(a.Type), FromStatus: a.FromStatus,
		ToStatus: a.ToStatus, Body: a.Body, OccurredAt: a.OccurredAt.UTC().Truncate(time.Microsecond),
		CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
	}
}

// Views

func (s *server) ListJobViews(ctx context.Context, _ gen.ListJobViewsRequestObject) (gen.ListJobViewsResponseObject, error) {
	views, err := s.jobs.ListViews(ctx, uid(ctx))
	if err != nil {
		return nil, err
	}
	out := make(gen.ListJobViews200JSONResponse, len(views))
	for i, v := range views {
		out[i] = toJobView(v)
	}
	return out, nil
}

func (s *server) CreateJobView(ctx context.Context, req gen.CreateJobViewRequestObject) (gen.CreateJobViewResponseObject, error) {
	v, err := s.jobs.CreateView(ctx, uid(ctx), viewInput(*req.Body))
	if err != nil {
		return nil, err
	}
	return gen.CreateJobView201JSONResponse(toJobView(v)), nil
}

func (s *server) PutJobView(ctx context.Context, req gen.PutJobViewRequestObject) (gen.PutJobViewResponseObject, error) {
	v, err := s.jobs.UpdateView(ctx, uid(ctx), req.Id, viewInput(*req.Body))
	if err != nil {
		return nil, err
	}
	return gen.PutJobView200JSONResponse(toJobView(v)), nil
}

func (s *server) DeleteJobView(ctx context.Context, req gen.DeleteJobViewRequestObject) (gen.DeleteJobViewResponseObject, error) {
	if err := s.jobs.DeleteView(ctx, uid(ctx), req.Id); err != nil {
		return nil, err
	}
	return gen.DeleteJobView204Response{}, nil
}

func viewInput(b gen.JobViewInput) jobs.ViewInput {
	in := jobs.ViewInput{Name: b.Name, Layout: string(deref(b.Layout)), Sort: jobs.Sort(deref(b.Sort))}
	if f := b.Filters; f != nil {
		in.Filters = jobs.ViewFilters{StatusIDs: deref(f.StatusIds), Q: deref(f.Q), IncludeArchived: deref(f.IncludeArchived)}
		for _, st := range deref(f.Stages) {
			in.Filters.Stages = append(in.Filters.Stages, string(st))
		}
	}
	return in
}

func toJobView(v jobs.View) gen.JobView {
	f := gen.JobViewFilters{}
	if len(v.Filters.StatusIDs) > 0 {
		f.StatusIds = &v.Filters.StatusIDs
	}
	if len(v.Filters.Stages) > 0 {
		st := make([]gen.JobStage, len(v.Filters.Stages))
		for i, x := range v.Filters.Stages {
			st[i] = gen.JobStage(x)
		}
		f.Stages = &st
	}
	if v.Filters.Q != "" {
		f.Q = &v.Filters.Q
	}
	if v.Filters.IncludeArchived {
		f.IncludeArchived = &v.Filters.IncludeArchived
	}
	return gen.JobView{
		Id: v.ID, Name: v.Name, Layout: gen.JobViewLayout(v.Layout), Filters: f, Sort: gen.JobSort(v.Sort),
		CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt,
	}
}
