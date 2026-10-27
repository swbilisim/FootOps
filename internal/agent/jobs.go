package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/swbilisim/FootOps/internal/run"
)

// Job is a long-running operation (site create/redeploy/remove, database copies).
// Requests return at once with a job ID; the panel polls the job. This keeps requests
// short, which matters behind Cloudflare (it cuts requests after 100 seconds) and for
// image pulls.
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

	// Bytes of Total processed so far; Total is 0 when the size is not known up front.
	Bytes int64 `json:"bytes,omitempty"`
	Total int64 `json:"total,omitempty"`
	// Usage is what the job's heavy commands used (highest memory, CPU time).
	Usage run.Usage `json:"usage"`
	// Result names what the job produced (e.g. a snapshot ID), for the panel's links.
	Result string `json:"result,omitempty"`

	Cancellable bool `json:"cancellable,omitempty"`
	Cancelled   bool `json:"cancelled,omitempty"`
	// Interrupted means the agent stopped while the job ran (restart, crash, reboot).
	Interrupted bool `json:"interrupted,omitempty"`
}

// JobSpec describes a job to start.
type JobSpec struct {
	Kind   string
	Target string
	Actor  Actor
	// Cancellable jobs can be stopped from the panel. Only jobs that clean up after
	// themselves on cancellation set it; site changes always run to the end or roll back.
	Cancellable bool
}

const (
	maxJobs = 50
	// progressSaveEvery limits disk writes for byte counters; steps are saved at once.
	progressSaveEvery = 5 * time.Second
)

// jobTimeouts are per kind; anything else gets defaultJobTimeout.
var (
	defaultJobTimeout = 20 * time.Minute
	jobTimeouts       = map[string]time.Duration{
		"db.snapshot":   time.Hour,
		"site.export":   time.Hour,
		"import.check":  time.Hour,
		"import.apply":  2 * time.Hour, // a copy, the restore and, on failure, the rollback
		"db.restore":    2 * time.Hour,
		"db.recover":    2 * time.Hour,
		"files.restore": time.Hour,
	}
)

// ErrBusy means another operation is still running.
var ErrBusy = errors.New("başka bir işlem sürüyor; bitince tekrar dene")

// Jobs runs one job at a time and remembers the last maxJobs. With Dir set, every job
// is also kept on disk, so a result survives an agent restart and a job cut short by
// one is reported as interrupted instead of vanishing.
type Jobs struct {
	Dir string

	mu      sync.Mutex
	m       map[string]*Job
	cancels map[string]context.CancelFunc
	order   []string
	busy    bool
	now     func() time.Time
	saved   map[string]time.Time // last disk write per job, for throttling
}

func NewJobs() *Jobs {
	return &Jobs{m: map[string]*Job{}, cancels: map[string]context.CancelFunc{}, saved: map[string]time.Time{}, now: time.Now}
}

var reJobFile = regexp.MustCompile(`^[A-Za-z0-9]{1,64}\.json$`)

// LoadJobs reads the jobs kept in dir. Jobs that were still running when the agent
// stopped are marked interrupted and returned, so the caller can record them.
func LoadJobs(dir string) (*Jobs, []Job, error) {
	j := NewJobs()
	j.Dir = dir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return j, nil, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return j, nil, err
	}
	var list []*Job
	for _, e := range ents {
		if !e.Type().IsRegular() || !reJobFile.MatchString(e.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var job Job
		if json.Unmarshal(b, &job) != nil || job.ID+".json" != e.Name() {
			continue
		}
		list = append(list, &job)
	}
	sort.Slice(list, func(a, b int) bool { return list[a].Started.Before(list[b].Started) })
	var interrupted []Job
	for _, job := range list {
		if !job.Done {
			job.Done, job.Interrupted, job.Finished = true, true, j.now().UTC()
			job.Err = "agent yeniden başladı; işlem yarıda kaldı"
			job.Steps = append(job.Steps, "✗ "+job.Err)
			j.save(job)
			interrupted = append(interrupted, *job)
		}
		j.m[job.ID] = job
		j.order = append(j.order, job.ID)
	}
	j.trim()
	return j, interrupted, nil
}

// Task is the handle a running job reports through.
type Task struct {
	j   *Jobs
	job *Job
}

// Step records a human-readable step. It is a Progress, so site operations can use it.
func (t *Task) Step(msg string) {
	t.j.mu.Lock()
	t.job.Steps = append(t.job.Steps, msg)
	t.j.saveLocked(t.job, true)
	t.j.mu.Unlock()
}

// SetBytes reports how much has been processed (total 0: unknown).
func (t *Task) SetBytes(done, total int64) {
	t.j.mu.Lock()
	t.job.Bytes, t.job.Total = done, total
	t.j.saveLocked(t.job, false)
	t.j.mu.Unlock()
}

// AddUsage folds a command's usage into the job's: the highest memory, summed CPU.
func (t *Task) AddUsage(u run.Usage) {
	t.j.mu.Lock()
	t.job.Usage = t.job.Usage.Add(u)
	t.j.mu.Unlock()
}

// Usage is what the job's commands have used so far.
func (t *Task) Usage() run.Usage {
	t.j.mu.Lock()
	defer t.j.mu.Unlock()
	return t.job.Usage
}

// SetResult records what the job produced.
func (t *Task) SetResult(r string) {
	t.j.mu.Lock()
	t.job.Result = r
	t.j.mu.Unlock()
}

// ID is the job's ID (also used to name its containers and staging directory).
func (t *Task) ID() string { return t.job.ID }

// Start runs fn in the background. Only one job runs at a time. onDone is called with
// the finished job (for the audit log).
func (j *Jobs) Start(spec JobSpec, fn func(ctx context.Context, t *Task) error, onDone func(Job)) (string, error) {
	j.mu.Lock()
	if j.busy {
		j.mu.Unlock()
		return "", ErrBusy
	}
	j.busy = true
	job := &Job{ID: randomToken(), Kind: spec.Kind, Target: spec.Target, Actor: spec.Actor,
		Started: j.now().UTC(), Cancellable: spec.Cancellable}
	timeout := defaultJobTimeout
	if d, ok := jobTimeouts[spec.Kind]; ok {
		timeout = d
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	j.m[job.ID] = job
	j.cancels[job.ID] = cancel
	j.order = append(j.order, job.ID)
	j.trim()
	j.saveLocked(job, true)
	j.mu.Unlock()

	go func() {
		defer cancel()
		t := &Task{j: j, job: job}
		err := fn(ctx, t)
		j.mu.Lock()
		job.Done, job.Finished = true, j.now().UTC()
		if err == nil && job.Cancelled {
			// The work was already done when the request came: report what happened.
			job.Cancelled = false
			job.Steps = append(job.Steps, "ℹ İptal istendi ama işlem o sırada zaten bitmişti")
		}
		if err != nil {
			switch {
			case job.Cancelled:
				job.Err = "iptal edildi"
			case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
				job.Err = fmt.Sprintf("zaman aşımı (%s): %v", timeout, err)
			default:
				job.Err = err.Error()
			}
		}
		delete(j.cancels, job.ID)
		j.busy = false
		j.saveLocked(job, true)
		snapshot := copyJob(job)
		j.mu.Unlock()
		if onDone != nil {
			onDone(snapshot)
		}
	}()
	return job.ID, nil
}

// Cancel stops a running job that allows it.
func (j *Jobs) Cancel(id string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	job, ok := j.m[id]
	switch {
	case !ok:
		return ErrJobNotFound
	case job.Done:
		return errors.New("işlem zaten bitti")
	case !job.Cancellable:
		return errors.New("bu işlem iptal edilemez; bitmesini bekle")
	}
	job.Cancelled = true
	job.Steps = append(job.Steps, "… iptal isteniyor")
	j.saveLocked(job, true)
	j.cancels[id]()
	return nil
}

// ErrJobNotFound means the job is unknown (or too old to be kept).
var ErrJobNotFound = errors.New("işlem bulunamadı")

// Get returns a copy of a job.
func (j *Jobs) Get(id string) (Job, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	job, ok := j.m[id]
	if !ok {
		return Job{}, false
	}
	return copyJob(job), true
}

// Running returns the job that is running, if any.
func (j *Jobs) Running() (Job, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for i := len(j.order) - 1; i >= 0; i-- {
		if job := j.m[j.order[i]]; !job.Done {
			return copyJob(job), true
		}
	}
	return Job{}, false
}

func copyJob(job *Job) Job {
	c := *job
	c.Steps = append([]string(nil), job.Steps...)
	return c
}

// trim forgets the oldest finished jobs beyond maxJobs. Callers hold j.mu.
func (j *Jobs) trim() {
	for len(j.order) > maxJobs {
		id := j.order[0]
		if !j.m[id].Done {
			return
		}
		j.order = j.order[1:]
		delete(j.m, id)
		delete(j.saved, id)
		if j.Dir != "" {
			_ = os.Remove(filepath.Join(j.Dir, id+".json"))
		}
	}
}

// saveLocked writes the job to disk; byte counters are throttled unless force is set.
// A failed write only costs the record surviving a restart, so it is not an error for
// the job itself. Callers hold j.mu.
func (j *Jobs) saveLocked(job *Job, force bool) {
	if j.Dir == "" {
		return
	}
	now := j.now()
	if !force && now.Sub(j.saved[job.ID]) < progressSaveEvery {
		return
	}
	j.saved[job.ID] = now
	j.save(job)
}

func (j *Jobs) save(job *Job) {
	b, err := json.Marshal(job)
	if err != nil {
		return
	}
	_ = writeFile(filepath.Join(j.Dir, job.ID+".json"), string(b), 0o600)
}

func randomToken() string {
	s, err := randomSecret(24)
	if err != nil {
		panic(err)
	}
	return s
}
