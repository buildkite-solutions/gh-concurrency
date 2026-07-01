package main

import (
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"time"
)

type progressReporter struct {
	mu             sync.Mutex
	out            io.Writer
	enabled        bool
	total          int
	started        int
	done           int
	totalJobs      int
	startedAt      time.Time
	targetPlural   string
	targetSingular string
}

func newProgressReporter(out io.Writer, enabled bool, total int) *progressReporter {
	return &progressReporter{
		out:            out,
		enabled:        enabled && out != nil,
		total:          total,
		startedAt:      time.Now(),
		targetPlural:   "repositories",
		targetSingular: "repo",
	}
}

func newProgressReporterForProvider(out io.Writer, enabled bool, total int, provider string) *progressReporter {
	progress := newProgressReporter(out, enabled, total)
	if provider == circleCIProvider {
		progress.targetPlural = "projects"
		progress.targetSingular = "project"
	}
	return progress
}

func (p *progressReporter) Begin() {
	if !p.enabled {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintf(p.out, "[gh-concurrency] %s queued: %d\n", p.targetPlural, p.total)
	fmt.Fprintf(p.out, "[gh-concurrency] progress: %s 0/%d\n", progressBar(0, p.total, 24), p.total)
}

func (p *progressReporter) Start(repo string) {
	if !p.enabled {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.started++
	fmt.Fprintf(p.out, "[gh-concurrency] examining %s %d/%d: %s\n", p.targetSingular, p.started, p.total, repo)
}

func (p *progressReporter) Done(repo string, jobs int) {
	if !p.enabled {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done++
	p.totalJobs += jobs
	fmt.Fprintf(p.out, "[gh-concurrency] progress: %s %d/%d done: %s (%d jobs, %d total)\n",
		progressBar(p.done, p.total, 24), p.done, p.total, repo, jobs, p.totalJobs)
}

func (p *progressReporter) Skip(repo string) {
	if !p.enabled {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done++
	fmt.Fprintf(p.out, "[gh-concurrency] progress: %s %d/%d skipped: %s (%d total jobs)\n",
		progressBar(p.done, p.total, 24), p.done, p.total, repo, p.totalJobs)
}

func (p *progressReporter) Complete() {
	if !p.enabled {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	elapsed := time.Since(p.startedAt).Round(time.Second)
	fmt.Fprintf(p.out, "[gh-concurrency] complete: %d/%d %s, %d jobs, %s elapsed\n",
		p.done, p.total, p.targetPlural, p.totalJobs, elapsed)
}

func progressBar(done, total, width int) string {
	if width < 1 {
		width = 1
	}
	if total < 1 {
		total = 1
	}
	if done < 0 {
		done = 0
	}
	if done > total {
		done = total
	}
	filled := int(math.Round(float64(done) / float64(total) * float64(width)))
	if filled > width {
		filled = width
	}
	return "[" + strings.Repeat("#", filled) + strings.Repeat("-", width-filled) + "]"
}
