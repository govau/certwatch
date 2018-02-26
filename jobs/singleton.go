package jobs

import (
	"errors"
	"log"
	"time"

	que "github.com/bgentry/que-go"
	"github.com/jackc/pgx"
)

var (
	ErrImmediateReschedule = errors.New("commit tx, and reschedule ASAP")
	ErrDidNotReschedule    = errors.New("no need to reschedule, we are done")
)

// JobFunc should do a thing. Return either:
// nil => wrapper will schedule the next cron (if a cron), then commit the tx.
// ErrImmediateReschedule => wrapper will commit the tx, then try it again immediately.
// ErrDidNotReschedule => wrapper will rollback the tx, and if a cron, will not reschedule or retry.
// any other error => wrapper rollback the tx, and allow que to reschedule
type JobFunc func(qc *que.Client, logger *log.Logger, job *que.Job, tx *pgx.Tx) error

type JobFuncWrapper struct {
	QC     *que.Client
	Logger *log.Logger
	F      JobFunc

	// One will only be called at once
	Singleton bool

	// Will be rescheduled upon success
	Duration time.Duration
}

// Return should continue, error. Never returns True if an error returns
func (scw *JobFuncWrapper) ensureNooneElseRunning(job *que.Job, tx *pgx.Tx, key string) (bool, error) {
	var lastCompleted time.Time
	var nextScheduled time.Time
	err := tx.QueryRow("SELECT last_completed, next_scheduled FROM cron_metadata WHERE id = $1 FOR UPDATE", key).Scan(&lastCompleted, &nextScheduled)
	if err != nil {
		if err == pgx.ErrNoRows {
			_, err = tx.Exec("INSERT INTO cron_metadata (id) VALUES ($1)", key)
			if err != nil {
				return false, err
			}
			return false, ErrImmediateReschedule
		}
		return false, err
	}

	if time.Now().Before(nextScheduled) {
		var futureJobs int
		// make sure we don't regard ourself as a future job. Sometimes clock skew makes us think we can't run yet.
		err = tx.QueryRow("SELECT count(*) FROM que_jobs WHERE job_class = $1 AND args::jsonb = $2::jsonb AND run_at >= $3 AND job_id != $4", job.Type, job.Args, nextScheduled, job.ID).Scan(&futureJobs)
		if err != nil {
			return false, err
		}

		if futureJobs > 0 {
			return false, nil
		}

		return false, scw.QC.EnqueueInTx(&que.Job{
			Type:  job.Type,
			Args:  job.Args,
			RunAt: nextScheduled,
		}, tx)
	}

	// Continue
	return true, nil
}

func (scw *JobFuncWrapper) scheduleJobLater(job *que.Job, tx *pgx.Tx, key string) error {
	n := time.Now()
	next := n.Add(scw.Duration)

	_, err := tx.Exec("UPDATE cron_metadata SET last_completed = $1, next_scheduled = $2 WHERE id = $3", n, next, key)
	if err != nil {
		tx.Rollback()
		return err
	}

	err = scw.QC.EnqueueInTx(&que.Job{
		Type:  job.Type,
		Args:  job.Args,
		RunAt: next,
	}, tx)
	if err != nil {
		return err
	}

	return nil
}

func (scw *JobFuncWrapper) Run(job *que.Job) error {
	for {
		err := scw.tryRun(job)
		switch err {
		case nil:
			return nil
		case ErrImmediateReschedule:
			scw.Logger.Printf("RESCHEDULE REQUESTED, RESTARTING... %s%s (%d)", job.Type, job.Args, job.ID)
			continue
		case ErrDidNotReschedule:
			scw.Logger.Printf("CRON JOB FINISHED AND HAS REQUESTED NOT TO BE RESCHEDULED %s%s (%d)", job.Type, job.Args, job.ID)
			return nil
		default:
			scw.Logger.Printf("FAILED WITH ERROR, RELY ON QUE TO RESCHEDULE %s%s (%d): %s", job.Type, job.Args, job.ID, err)
			return err
		}
	}
}

// This job manages the tx, no one else should commit or rollback
func (scw *JobFuncWrapper) tryRun(job *que.Job) error {
	scw.Logger.Printf("START %s%s (%d)", job.Type, job.Args, job.ID)
	defer scw.Logger.Printf("STOP %s%s (%d)", job.Type, job.Args, job.ID)

	tx, err := job.Conn().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	key := job.Type + string(job.Args)
	if scw.Singleton {
		carryOn, err := scw.ensureNooneElseRunning(job, tx, key)
		if !carryOn {
			// We are not carrying on, check the error codes.
			switch err {
			case nil:
				return tx.Commit()
			case ErrImmediateReschedule:
				// We commit, but propagate the error code
				err = tx.Commit()
				if err != nil {
					return err
				}
				return ErrImmediateReschedule
			default:
				return err
			}
		}
	}

	err = scw.F(scw.QC, scw.Logger, job, tx)
	switch err {
	case nil:
		// continue, we commit later
	case ErrImmediateReschedule:
		err = tx.Commit()
		if err != nil {
			return err
		}
		return ErrImmediateReschedule
	default:
		return err
	}

	if scw.Singleton && scw.Duration != 0 {
		err = scw.scheduleJobLater(job, tx, key)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}
