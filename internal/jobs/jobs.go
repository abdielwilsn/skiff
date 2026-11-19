// Package jobs runs long server tasks (provisioning, deploys) in the background,
// one at a time, writing each job's output to a log file the UI can tail.
package jobs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"skiff/internal/store"
)

// Func does the work of a job, writing progress to log.
type Func func(ctx context.Context, log io.Writer) error

// Timeout bounds a single job so a hung command can't block the queue forever.
const Timeout = 30 * time.Minute

type item struct {
	id   int64
	fn   Func
	done func(error)
}

// Runner executes jobs sequentially. Running one job at a time keeps memory
// use flat on tiny servers and avoids apt/composer lock contention.
type Runner struct {
	store  *store.Store
	logDir string
	ctx    context.Context

	mu    sync.Mutex
	queue []item
	wake  chan struct{}
}

func NewRunner(ctx context.Context, st *store.Store, logDir string) *Runner {
	r := &Runner{store: st, logDir: logDir, ctx: ctx, wake: make(chan struct{}, 1)}
	go r.loop()
	return r
}

// Enqueue records a job and schedules it. done (optional) is called with the
// job's result before its final status is saved.
func (r *Runner) Enqueue(siteID *int64, kind, title string, fn Func, done func(error)) (int64, error) {
	return r.EnqueueDetail(siteID, kind, title, "", fn, done)
}

// EnqueueDetail is Enqueue with extra text stored on the job (shown in history).
func (r *Runner) EnqueueDetail(siteID *int64, kind, title, detail string, fn Func, done func(error)) (int64, error) {
	id, err := r.store.CreateJob(siteID, kind, title, detail)
	if err != nil {
		return 0, err
	}
	r.mu.Lock()
	r.queue = append(r.queue, item{id: id, fn: fn, done: done})
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
	return id, nil
}

func (r *Runner) next() (item, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.queue) == 0 {
		return item{}, false
	}
	it := r.queue[0]
	r.queue = r.queue[1:]
	return it, true
}

func (r *Runner) loop() {
	for {
		for {
			it, ok := r.next()
			if !ok {
				break
			}
			r.run(it)
		}
		select {
		case <-r.ctx.Done():
			return
		case <-r.wake:
		}
	}
}

func (r *Runner) LogPath(id int64) string {
	return filepath.Join(r.logDir, fmt.Sprintf("job-%d.log", id))
}

func (r *Runner) run(it item) {
	finish := func(err error) {
		if it.done != nil {
			it.done(err)
		}
		status := store.JobSuccess
		if err != nil {
			status = store.JobFailed
		}
		if serr := r.store.SetJobStatus(it.id, status); serr != nil {
			slog.Error("save job status", "job", it.id, "err", serr)
		}
	}

	f, err := os.OpenFile(r.LogPath(it.id), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		slog.Error("open job log", "job", it.id, "err", err)
		finish(err)
		return
	}
	defer f.Close()

	if err := r.store.SetJobStatus(it.id, store.JobRunning); err != nil {
		slog.Error("save job status", "job", it.id, "err", err)
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(r.ctx, Timeout)
	err = safeRun(ctx, it.fn, f)
	cancel()

	took := time.Since(start).Round(time.Second)
	if err != nil {
		fmt.Fprintf(f, "\n✗ Failed after %s: %v\n", took, err)
	} else {
		fmt.Fprintf(f, "\n✓ Finished in %s\n", took)
	}
	finish(err)
}

func safeRun(ctx context.Context, fn Func, w io.Writer) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("internal error: %v", p)
		}
	}()
	return fn(ctx, w)
}

// ReadLog returns up to 256 KiB of a job's log starting at offset, plus the
// file's current size.
func (r *Runner) ReadLog(id, offset int64) (data []byte, size int64, err error) {
	f, err := os.Open(r.LogPath(id))
	if os.IsNotExist(err) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size = st.Size()
	if offset >= size {
		return nil, size, nil
	}
	n := size - offset
	if n > 256<<10 {
		n = 256 << 10
	}
	data = make([]byte, n)
	read, err := f.ReadAt(data, offset)
	if err != nil && err != io.EOF {
		return nil, size, err
	}
	return data[:read], size, nil
}
