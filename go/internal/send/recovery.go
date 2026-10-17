package send

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/zero11-0203/s3mail/go/internal/model"
)

// Recovery is the one-minute work-recovery sweep: it settles expired
// attempt leases and re-enqueues due work that no wake-up is carrying.
// It never talks to the relay.
type Recovery struct {
	Store  Store
	Config interface {
		Paused(ctx context.Context) (bool, error)
	}
	Queue Queue
	Log   *slog.Logger
	Now   func() time.Time
}

// Grace is how long a due submission may stay queued before the sweep
// assumes its wake-up was lost.
const Grace = 2 * time.Minute

func (r *Recovery) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Run performs one sweep.
func (r *Recovery) Run(ctx context.Context) error {
	subs, err := r.Store.ListSubmissions(ctx)
	if err != nil {
		return err
	}
	paused, err := r.Config.Paused(ctx)
	if err != nil {
		return err
	}
	var errs []error
	unknown := 0
	for _, s := range subs {
		now := r.now()
		if s.Status == model.SubmissionUnknown {
			unknown++
		}
		switch {
		case s.Status == model.SubmissionActive && !now.Before(s.LeaseUntil):
			after, done, err := commit(ctx, r.Store, s.ID, func(cur model.Submission) (*model.Mutation, error) {
				return Recover(cur, r.now()), nil
			})
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if done {
				r.Log.Warn("lease expired", "submissionId", s.ID, "status", after.Status)
				if after.Status == model.SubmissionUnknown {
					unknown++
					r.Log.Error("acceptance unknown", "submissionId", s.ID, "outcome", "acceptance_unknown")
				}
				if after.Status == model.SubmissionQueued && !paused {
					errs = append(errs, r.Queue.Enqueue(ctx, s.ID, 0))
				}
			}
		case s.Status == model.SubmissionQueued && !paused && !now.Before(s.NextAttemptAt.Add(Grace)):
			errs = append(errs, r.Queue.Enqueue(ctx, s.ID, 0))
		}
	}
	r.gauge(unknown)
	return errors.Join(errs...)
}

// MetricNamespace holds the custom mail metrics
// (docs/operations/monitoring.md#metrics-cloudwatch).
const MetricNamespace = "Mail"

// gauge logs the number of submissions waiting for the owner to resolve
// unknown acceptance as an Embedded Metric Format record, once a sweep, so
// its alarm stays raised until every one is resolved. The production
// logger adds the component dimension.
func (r *Recovery) gauge(unknown int) {
	r.Log.Info("unresolved submissions",
		"_aws", map[string]any{
			"Timestamp": r.now().UnixMilli(),
			"CloudWatchMetrics": []map[string]any{{
				"Namespace":  MetricNamespace,
				"Dimensions": [][]string{{"component"}},
				"Metrics":    []map[string]string{{"Name": "SubmissionsUnknown", "Unit": "Count"}},
			}},
		},
		"SubmissionsUnknown", unknown)
}
