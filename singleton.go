package main

import (
	"errors"
	"log"
	"time"

	que "github.com/bgentry/que-go"
	"github.com/jackc/pgx"
)

var (
	ErrTryAgainPlease  = errors.New("try again, now we have metadata")
	ErrDoNotReschedule = errors.New("no need to reschedule, we are done")
)

type JobFunc func(qc *que.Client, logger *log.Logger, job *que.Job, tx *pgx.Tx) error

type JobFuncWrapper struct {
	QC        *que.Client
	Logger    *log.Logger
	F         JobFunc
	Singleton bool
	Duration  time.Duration
}

// Return should continue, error
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
			err = tx.Commit()
			if err != nil {
				return false, err
			}
			return false, ErrTryAgainPlease
		}
		return false, err
	}

	scw.Logger.Println("got mutex", job.ID)
	if time.Now().Before(nextScheduled) {
		var futureJobs int
		err = tx.QueryRow("SELECT count(*) FROM que_jobs WHERE job_class = $1 AND args::jsonb = $2::jsonb AND run_at >= $3", job.Type, job.Args, nextScheduled).Scan(&futureJobs)
		if err != nil {
			return false, err
		}

		if futureJobs > 0 {
			scw.Logger.Println("Enough future jobs already scheduled.", job.ID)
			return false, nil
		}

		scw.Logger.Println("No future jobs found, scheduling one to match end time", job.ID)
		err = scw.QC.EnqueueInTx(&que.Job{
			Type:  job.Type,
			Args:  job.Args,
			RunAt: nextScheduled,
		}, tx)
		if err != nil {
			return false, err
		}
		return false, tx.Commit()
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
	tx, err := job.Conn().Begin()
	if err != nil {
		return err
	}
	// Per docs it is safe call rollback() as a noop on an already closed transaction
	defer tx.Rollback()

	scw.Logger.Println("starting", job.ID)
	defer scw.Logger.Println("stop", job.ID)

	key := job.Type + string(job.Args)
	if scw.Singleton {
		carryOn, err := scw.ensureNooneElseRunning(job, tx, key)
		if err != nil {
			return err
		}

		// If we are running ahead of schedule, see ya later.
		if !carryOn {
			return nil
		}
	}

	err = scw.F(scw.QC, scw.Logger, job, tx)
	if err != nil {
		if err == ErrDoNotReschedule {
			scw.Logger.Println("job has requested to NOT be rescheduled", job.ID)
			return nil
		}
		return err
	}

	if scw.Singleton {
		err = scw.scheduleJobLater(job, tx, key)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}
