package agent

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Job is a long-running operation (site create/redeploy/remove). Requests return at
// once with a job ID; the panel polls the job. This keeps requests short, which matters
// behind Cloudflare (it cuts requests after 100 seconds) and for image pulls.
type Job struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`
	Target   string    `json:"target"`
	Actor    Actor     `json:"actor"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitempty"`
	Steps    []string  `json:"steps"`
	Done     bool      `json:"done"`
	Err      string    `json:"error,omitempty"`
}

const (
	maxJobs    = 50
	jobTimeout = 20 * time.Minute
)

// ErrBusy means another operation is still running.
var ErrBusy = errors.New("başka bir işlem sürüyor; bitince tekrar dene")

type Jobs struct {
	mu    sync.Mutex
	m     map[string]*Job
	order []string
	busy  bool
	now   func() time.Time
}

func NewJobs() *Jobs { return &Jobs{m: map[string]*Job{}, now: time.Now} }

// Start runs fn in the background. Only one job runs at a time. onDone is called with
// the finished job (for the audit log).
func (j *Jobs) Start(kind, target string, actor Actor, fn func(ctx context.Context, p Progress) error, onDone func(Job)) (string, error) {
	j.mu.Lock()
	if j.busy {
		j.mu.Unlock()
		return "", ErrBusy
	}
	j.busy = true
	job := &Job{ID: randomToken(), Kind: kind, Target: target, Actor: actor, Started: j.now().UTC()}
	j.m[job.ID] = job
	j.order = append(j.order, job.ID)
	if len(j.order) > maxJobs {
		delete(j.m, j.order[0])
		j.order = j.order[1:]
	}
	j.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
		defer cancel()
		err := fn(ctx, func(msg string) {
			j.mu.Lock()
			job.Steps = append(job.Steps, msg)
			j.mu.Unlock()
		})
		j.mu.Lock()
		job.Done, job.Finished = true, j.now().UTC()
		if err != nil {
			job.Err = err.Error()
		}
		j.busy = false
		snapshot := *job
		snapshot.Steps = append([]string(nil), job.Steps...)
		j.mu.Unlock()
		if onDone != nil {
			onDone(snapshot)
		}
	}()
	return job.ID, nil
}

// Get returns a copy of a job.
func (j *Jobs) Get(id string) (Job, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	job, ok := j.m[id]
	if !ok {
		return Job{}, false
	}
	c := *job
	c.Steps = append([]string(nil), job.Steps...)
	return c, true
}

func randomToken() string {
	s, err := randomSecret(24)
	if err != nil {
		panic(err)
	}
	return s
}
