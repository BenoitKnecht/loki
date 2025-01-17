package ingester

import (
	"fmt"
	"math"
	"sync"

	cortex_limiter "github.com/cortexproject/cortex/pkg/util/limiter"
	"golang.org/x/time/rate"

	"github.com/grafana/loki/pkg/validation"
)

const (
	errMaxStreamsPerUserLimitExceeded = "tenant '%v' per-user streams limit exceeded, streams: %d exceeds calculated limit: %d (local limit: %d, global limit: %d, global/ingesters: %d)"
)

// RingCount is the interface exposed by a ring implementation which allows
// to count members
type RingCount interface {
	HealthyInstancesCount() int
}

// Limiter implements primitives to get the maximum number of streams
// an ingester can handle for a specific tenant
type Limiter struct {
	limits            *validation.Overrides
	ring              RingCount
	replicationFactor int
	metrics           *ingesterMetrics

	mtx      sync.RWMutex
	disabled bool
}

func (l *Limiter) DisableForWALReplay() {
	l.mtx.Lock()
	defer l.mtx.Unlock()
	l.disabled = true
	l.metrics.limiterEnabled.Set(0)
}

func (l *Limiter) Enable() {
	l.mtx.Lock()
	defer l.mtx.Unlock()
	l.disabled = false
	l.metrics.limiterEnabled.Set(1)
}

// NewLimiter makes a new limiter
func NewLimiter(limits *validation.Overrides, metrics *ingesterMetrics, ring RingCount, replicationFactor int) *Limiter {
	return &Limiter{
		limits:            limits,
		ring:              ring,
		replicationFactor: replicationFactor,
		metrics:           metrics,
	}
}

func (l *Limiter) UnorderedWrites(userID string) bool {
	// WAL replay should not discard previously ack'd writes,
	// so allow out of order writes while the limiter is disabled.
	if l.disabled {
		return true
	}
	return l.limits.UnorderedWrites(userID)
}

// AssertMaxStreamsPerUser ensures limit has not been reached compared to the current
// number of streams in input and returns an error if so.
func (l *Limiter) AssertMaxStreamsPerUser(userID string, streams int) error {
	// Until the limiter actually starts, all accesses are successful.
	// This is used to disable limits while recovering from the WAL.
	l.mtx.RLock()
	defer l.mtx.RUnlock()
	if l.disabled {
		return nil
	}

	// Start by setting the local limit either from override or default
	localLimit := l.limits.MaxLocalStreamsPerUser(userID)

	// We can assume that streams are evenly distributed across ingesters
	// so we do convert the global limit into a local limit
	globalLimit := l.limits.MaxGlobalStreamsPerUser(userID)
	adjustedGlobalLimit := l.convertGlobalToLocalLimit(globalLimit)

	// Set the calculated limit to the lesser of the local limit or the new calculated global limit
	calculatedLimit := l.minNonZero(localLimit, adjustedGlobalLimit)

	// If both the local and global limits are disabled, we just
	// use the largest int value
	if calculatedLimit == 0 {
		calculatedLimit = math.MaxInt32
	}

	if streams < calculatedLimit {
		return nil
	}

	return fmt.Errorf(errMaxStreamsPerUserLimitExceeded, userID, streams, calculatedLimit, localLimit, globalLimit, adjustedGlobalLimit)
}

func (l *Limiter) convertGlobalToLocalLimit(globalLimit int) int {
	if globalLimit == 0 {
		return 0
	}

	// Given we don't need a super accurate count (ie. when the ingesters
	// topology changes) and we prefer to always be in favor of the tenant,
	// we can use a per-ingester limit equal to:
	// (global limit / number of ingesters) * replication factor
	numIngesters := l.ring.HealthyInstancesCount()

	// May happen because the number of ingesters is asynchronously updated.
	// If happens, we just temporarily ignore the global limit.
	if numIngesters > 0 {
		return int((float64(globalLimit) / float64(numIngesters)) * float64(l.replicationFactor))
	}

	return 0
}

func (l *Limiter) minNonZero(first, second int) int {
	if first == 0 || (second != 0 && first > second) {
		return second
	}

	return first
}

type localStrategy struct {
	limiter *Limiter
}

func newLocalStreamRateStrategy(l *Limiter) cortex_limiter.RateLimiterStrategy {
	return &localStrategy{
		limiter: l,
	}
}

func (s *localStrategy) Limit(userID string) float64 {
	if s.limiter.disabled {
		return float64(rate.Inf)
	}
	return float64(s.limiter.limits.MaxLocalStreamRateBytes(userID))
}

func (s *localStrategy) Burst(userID string) int {
	if s.limiter.disabled {
		// Burst is ignored when limit = rate.Inf
		return 0
	}
	return s.limiter.limits.MaxLocalStreamBurstRateBytes(userID)
}
