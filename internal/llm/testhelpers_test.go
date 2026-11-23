package llm

import (
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/sony/gobreaker"
	"golang.org/x/sync/semaphore"
)

func newSemForTest() *semaphore.Weighted { return semaphore.NewWeighted(2) }
func newBreakerForTest() *gobreaker.CircuitBreaker {
	return gobreaker.NewCircuitBreaker(gobreaker.Settings{Name: "test", MaxRequests: 2})
}
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}
func timeNow() time.Time { return time.Now() }

var _ = sync.Mutex{}
