// Package scheduler starts sync runs from cron, on start and on demand,
// with only one run at a time (spec section 5).
package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	npmsync "github.com/toyonix/nginx-proxy-manager-sync/internal/sync"
)

// RunFunc performs one sync run.
type RunFunc func(ctx context.Context, trigger string, opts npmsync.RunOptions) *npmsync.RunResult

// Scheduler serialises runs and keeps the last result.
type Scheduler struct {
	run  RunFunc
	log  *slog.Logger
	spec string

	cron    *cron.Cron
	entryID cron.EntryID

	mu      sync.Mutex
	running bool
	last    *npmsync.RunResult
	wg      sync.WaitGroup
	ctx     context.Context

	// OnResult, if set, is called after every run (for metrics).
	OnResult func(*npmsync.RunResult)
}

// New returns a scheduler. cronSpec may be empty to disable scheduled runs.
func New(cronSpec string, run RunFunc, log *slog.Logger) *Scheduler {
	return &Scheduler{run: run, log: log, spec: cronSpec}
}

// Start starts the cron schedule. Runs started from cron and Trigger use
// ctx; cancelling it stops a run in progress.
func (s *Scheduler) Start(ctx context.Context) error {
	s.ctx = ctx
	if s.spec == "" {
		s.log.Info("scheduled runs disabled (cron is empty)")
		return nil
	}
	s.cron = cron.New(cron.WithParser(cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)))
	id, err := s.cron.AddFunc(s.spec, func() { s.Trigger("cron", npmsync.RunOptions{}) })
	if err != nil {
		return err
	}
	s.entryID = id
	s.cron.Start()
	s.log.Info("scheduled runs enabled", "cron", s.spec, "next", s.Next().Format(time.RFC3339))
	return nil
}

// Trigger starts a run in the background. It returns false, and starts
// nothing, if a run is already in progress.
func (s *Scheduler) Trigger(trigger string, opts npmsync.RunOptions) bool {
	if !s.begin(trigger) {
		return false
	}
	s.wg.Go(func() {
		s.execute(s.ctx, trigger, opts)
	})
	return true
}

// RunNow runs synchronously and returns the result, or nil if a run is
// already in progress.
func (s *Scheduler) RunNow(ctx context.Context, trigger string, opts npmsync.RunOptions) *npmsync.RunResult {
	if !s.begin(trigger) {
		return nil
	}
	return s.execute(ctx, trigger, opts)
}

func (s *Scheduler) begin(trigger string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		s.log.Warn("sync trigger ignored: a run is already in progress", "trigger", trigger)
		return false
	}
	s.running = true
	return true
}

func (s *Scheduler) execute(ctx context.Context, trigger string, opts npmsync.RunOptions) *npmsync.RunResult {
	if ctx == nil {
		ctx = context.Background()
	}
	res := s.run(ctx, trigger, opts)
	s.mu.Lock()
	s.running = false
	s.last = res
	s.mu.Unlock()
	if s.OnResult != nil {
		s.OnResult(res)
	}
	if s.cron != nil {
		s.log.Info("next scheduled run", "at", s.Next().Format(time.RFC3339))
	}
	return res
}

// Running reports whether a run is in progress.
func (s *Scheduler) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Last returns the result of the last finished run, or nil.
func (s *Scheduler) Last() *npmsync.RunResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// Next returns the time of the next scheduled run (zero if none).
func (s *Scheduler) Next() time.Time {
	if s.cron == nil {
		return time.Time{}
	}
	return s.cron.Entry(s.entryID).Next
}

// Stop stops the schedule and waits for a run in progress to finish.
func (s *Scheduler) Stop() {
	if s.cron != nil {
		<-s.cron.Stop().Done()
	}
	s.wg.Wait()
}
