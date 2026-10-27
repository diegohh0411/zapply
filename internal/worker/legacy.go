package worker

import (
	"context"

	"github.com/riverqueue/river"
)

// legacyArgs reads persisted jobs created before migration 11. New requests
// always use Args and the model_operation kind, including decision requests.
type legacyArgs Args

func (legacyArgs) Kind() string { return "llm_diagnostic" }

type legacyWorker struct {
	river.WorkerDefaults[legacyArgs]
	runtime *Runtime
}

func (w *legacyWorker) Work(ctx context.Context, j *river.Job[legacyArgs]) error {
	return w.runtime.Work(ctx, &river.Job[Args]{JobRow: j.JobRow, Args: Args(j.Args)})
}
