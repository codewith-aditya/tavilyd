package research

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/astralabs/tavilyd/internal/config"
	"github.com/astralabs/tavilyd/internal/llm"
)

// Manager owns the worker pool, the in-memory live view and SSE subscribers.
type Manager struct {
	cfg   *config.Config
	llm   *llm.Client
	tools Tools
	store *Store
	log   *slog.Logger

	queue  chan *Job
	mu     sync.Mutex
	active map[string]*activeJob
	jobs   map[string]*Job
	subs   map[string]map[chan Event]struct{}
	counts map[string]int
	wg     sync.WaitGroup
	root   context.Context
	cancel context.CancelFunc
}

type activeJob struct {
	cancel context.CancelFunc
}

// NewManager builds the manager; call Start to launch workers.
func NewManager(cfg *config.Config, client *llm.Client, tools Tools, store *Store, log *slog.Logger) *Manager {
	return &Manager{
		cfg: cfg, llm: client, tools: tools, store: store, log: log,
		queue:  make(chan *Job, cfg.ResearchQueue),
		active: make(map[string]*activeJob),
		jobs:   make(map[string]*Job),
		subs:   make(map[string]map[chan Event]struct{}),
		counts: make(map[string]int),
	}
}

// Start launches the worker pool.
func (m *Manager) Start(parent context.Context) {
	m.root, m.cancel = context.WithCancel(parent)
	for i := 0; i < m.cfg.ResearchWorkers; i++ {
		m.wg.Add(1)
		go m.worker(i)
	}
}

// Close stops workers and fails queued jobs.
func (m *Manager) Close() {
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()
}

// Create registers a job and subscribes to its events (stream mode).
func (m *Manager) Create(ctx context.Context, job *Job) (<-chan Event, func(), error) {
	if err := m.store.Ping(ctx); err != nil {
		return nil, nil, ErrUnavailable
	}
	m.mu.Lock()
	if m.counts[job.Key] >= m.cfg.ResearchMaxJobsPerKey {
		m.mu.Unlock()
		return nil, nil, ErrBusy
	}
	m.counts[job.Key]++
	job.Status = StatusPending
	job.CreatedAt = time.Now().UTC()
	if err := m.store.Save(ctx, job); err != nil {
		m.counts[job.Key]--
		m.mu.Unlock()
		return nil, nil, ErrUnavailable
	}
	m.jobs[job.ID] = job
	ch := make(chan Event, 32)
	if m.subs[job.ID] == nil {
		m.subs[job.ID] = make(map[chan Event]struct{})
	}
	m.subs[job.ID][ch] = struct{}{}
	m.mu.Unlock()

	unsub := func() {
		m.mu.Lock()
		if subs, ok := m.subs[job.ID]; ok {
			delete(subs, ch)
			if len(subs) == 0 {
				delete(m.subs, job.ID)
			}
		}
		m.mu.Unlock()
	}

	select {
	case m.queue <- job:
		return ch, unsub, nil
	default:
		unsub()
		m.mu.Lock()
		m.counts[job.Key]--
		delete(m.jobs, job.ID)
		m.mu.Unlock()
		return nil, nil, ErrBusy
	}
}

// Cancel aborts a running job (client disconnect).
func (m *Manager) Cancel(id string) {
	m.mu.Lock()
	aj := m.active[id]
	m.mu.Unlock()
	if aj != nil && aj.cancel != nil {
		aj.cancel()
	}
}

// Get returns the live job if present, else the stored one.
func (m *Manager) Get(ctx context.Context, id string) (*Job, error) {
	m.mu.Lock()
	job := m.jobs[id]
	m.mu.Unlock()
	if job != nil {
		cp := *job
		return &cp, nil
	}
	return m.store.Get(ctx, id)
}

func (m *Manager) worker(n int) {
	defer m.wg.Done()
	for {
		select {
		case <-m.root.Done():
			return
		case job, ok := <-m.queue:
			if !ok {
				return
			}
			m.run(job)
		}
	}
}

func (m *Manager) run(job *Job) {
	tier := MiniTier()
	deadline := m.cfg.ResearchDeadlineMini
	if job.Model == "pro" {
		tier = ProTier()
		deadline = m.cfg.ResearchDeadlinePro
	}
	tier.Deadline = deadline
	tier.MaxLLM = m.cfg.ResearchMaxLLMCallsMini
	tier.MaxTokens = m.cfg.ResearchMaxTokensMini
	tier.Pages = m.cfg.ResearchMaxPagesMini
	if job.Model == "pro" {
		tier.MaxLLM = m.cfg.ResearchMaxLLMCallsPro
		tier.MaxTokens = m.cfg.ResearchMaxTokensPro
		tier.Pages = m.cfg.ResearchMaxPagesPro
	}

	ctx, cancel := context.WithTimeout(m.root, deadline)
	defer cancel()
	m.mu.Lock()
	m.active[job.ID] = &activeJob{cancel: cancel}
	job.Status = StatusInProgress
	m.mu.Unlock()
	_ = m.store.Save(context.Background(), job)
	m.publish(job.ID, Event{Type: "progress", Stage: "planning", Detail: "job started"})

	engine := NewEngine(m.llm, m.tools, tier, m.log)
	start := time.Now()
	err := engine.Run(ctx, job, func(ev Event) { m.publish(job.ID, ev) })
	job.ResponseTime = time.Since(start).Seconds()
	if err != nil {
		job.Status = StatusFailed
		job.Error = err.Error()
	} else {
		job.Status = StatusCompleted
	}
	_ = m.store.Save(context.Background(), job)

	m.mu.Lock()
	delete(m.active, job.ID)
	if m.counts[job.Key] > 0 {
		m.counts[job.Key]--
	}
	m.mu.Unlock()

	if err != nil {
		m.publish(job.ID, Event{Type: "error", Error: job.Error})
		return
	}
	events := job.Content
	// Send the report in chunks so clients can render progressively.
	for len(events) > 0 {
		n := 400
		if len(events) < n {
			n = len(events)
		}
		m.publish(job.ID, Event{Type: "delta", Delta: events[:n]})
		events = events[n:]
	}
	m.publish(job.ID, Event{Type: "done", Sources: job.Sources, ResponseTime: job.ResponseTime})
}

func (m *Manager) publish(id string, ev Event) {
	m.mu.Lock()
	subs := m.subs[id]
	channels := make([]chan Event, 0, len(subs))
	for ch := range subs {
		channels = append(channels, ch)
	}
	m.mu.Unlock()
	for _, ch := range channels {
		select {
		case ch <- ev:
		default: // slow subscriber: drop rather than block the job
		}
	}
}

// Health reports Redis availability for /research.
func (m *Manager) Health(ctx context.Context) error {
	return m.store.Ping(ctx)
}

// JobErrorText is used by the API for a friendly failure message.
func JobErrorText(err error) string {
	switch {
	case errors.Is(err, ErrUnavailable):
		return "research storage unavailable"
	case errors.Is(err, ErrBusy):
		return "research queue is full"
	default:
		return fmt.Sprintf("%v", err)
	}
}
