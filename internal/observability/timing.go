package observability

import (
	"sync"
	"time"
)

// Stage names used in X-Timing and Prometheus labels.
const (
	StageCache  = "cache"
	StageSearch = "search"
	StageTier1  = "tier1"
	StageTier2  = "tier2"
	StageRerank = "rerank"
	StageTotal  = "total"
)

var stageOrder = []string{StageCache, StageSearch, StageTier1, StageTier2, StageRerank, StageTotal}

// Timings accumulates per-stage durations for one request. It is safe for
// concurrent use because extraction workers update it from many goroutines.
type Timings struct {
	mu     sync.Mutex
	stages map[string]time.Duration
	start  time.Time
}

// NewTimings starts a timer at now.
func NewTimings() *Timings {
	return &Timings{stages: make(map[string]time.Duration), start: time.Now()}
}

// Add records d under stage, accumulating repeated stages.
func (t *Timings) Add(stage string, d time.Duration) {
	if d <= 0 {
		return
	}
	t.mu.Lock()
	t.stages[stage] += d
	t.mu.Unlock()
}

// Track measures the time spent inside fn under stage.
func (t *Timings) Track(stage string, fn func()) {
	start := time.Now()
	fn()
	t.Add(stage, time.Since(start))
}

// Snapshot returns a copy of the recorded durations plus the elapsed total.
func (t *Timings) Snapshot() map[string]time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]time.Duration, len(t.stages)+1)
	for k, v := range t.stages {
		out[k] = v
	}
	out[StageTotal] = time.Since(t.start)
	return out
}

// Header renders the timings as key=milliseconds pairs in a stable order.
func (t *Timings) Header() string {
	snap := t.Snapshot()
	buf := make([]byte, 0, 96)
	for i, name := range stageOrder {
		if i > 0 {
			buf = append(buf, ';')
		}
		buf = append(buf, name...)
		buf = append(buf, '=')
		buf = appendInt(buf, int64(snap[name]/time.Millisecond))
	}
	return string(buf)
}

func appendInt(b []byte, v int64) []byte {
	if v == 0 {
		return append(b, '0')
	}
	var tmp [20]byte
	i := len(tmp)
	neg := v < 0
	if neg {
		v = -v
	}
	for v > 0 {
		i--
		tmp[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		tmp[i] = '-'
	}
	return append(b, tmp[i:]...)
}
