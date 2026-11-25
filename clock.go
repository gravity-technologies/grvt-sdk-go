package grvt

import (
	"crypto/rand"
	"encoding/binary"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gravity-technologies/grvt-sdk-go/internal/behavior"
)

// Clock gives the time used for signature expirations. Client options so tests can pin it.
type Clock interface {
	// NowNs is nanoseconds since the Unix epoch.
	NowNs() int64
}

// NonceProvider gives the uint32 signature nonce (the EIP-712 Order.nonce is uint32).
type NonceProvider interface {
	NextNonce() uint32
}

// ClientOrderIDProvider gives client_order_id values for orders that have none.
type ClientOrderIDProvider interface {
	NextClientOrderID() uint64
}

// SystemClock is the local wall clock plus an optional offset to the server clock. Observe (e.g. with
// GET /time, see Runtime.SyncClock) moves the offset toward the server with exponential smoothing.
type SystemClock struct {
	mu      sync.Mutex
	offset  atomic.Int64
	samples int
}

// Observe feeds one server time sample (unix ms) and the round trip it took.
func (c *SystemClock) Observe(serverMs int64, rtt time.Duration) {
	localMid := time.Now().UnixNano() - int64(rtt/2)
	sample := serverMs*1_000_000 - localMid
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.samples == 0 {
		c.offset.Store(sample)
	} else {
		c.offset.Store(int64(float64(c.offset.Load())*0.8 + float64(sample)*0.2))
	}
	c.samples++
}

// OffsetNs is the current estimate of server time minus local time.
func (c *SystemClock) OffsetNs() int64 { return c.offset.Load() }

func (c *SystemClock) NowNs() int64 { return time.Now().UnixNano() + c.offset.Load() }

// FixedClock always returns the same time.
type FixedClock int64

func (c FixedClock) NowNs() int64 { return int64(c) }

// RandomNonce draws nonces from crypto/rand.
type RandomNonce struct{}

func (RandomNonce) NextNonce() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.LittleEndian.Uint32(b[:])
}

var defaultClientOrderIDs TimeCounterClientOrderID

// TimeCounterClientOrderID generates IDs from the Unix second and a randomly seeded 31-bit counter.
// By default, all clients in the process share one instance.
type TimeCounterClientOrderID struct {
	once    sync.Once
	counter atomic.Uint32
	random  func() uint32 // nil: crypto/rand
}

func (p *TimeCounterClientOrderID) NextClientOrderID() uint64 {
	p.once.Do(func() {
		random := p.random
		if random == nil {
			random = RandomNonce{}.NextNonce
		}
		p.counter.Store(random())
	})
	seconds := uint64(time.Now().Unix()) & behavior.ClientOrderIDSecondsMask
	return 1<<63 | seconds<<behavior.ClientOrderIDCounterBits | uint64((p.counter.Add(1)-1)&behavior.ClientOrderIDCounterMask)
}
