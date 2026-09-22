package power

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBackend counts starts and lets a test end an assertion from outside.
type fakeBackend struct {
	mu      sync.Mutex
	starts  int
	fail    error
	live    *fakeAssertion
	power   string
	stopped int
}

type fakeAssertion struct {
	b    *fakeBackend
	done chan struct{}
	once sync.Once
}

func (b *fakeBackend) Name() string { return "fake" }
func (b *fakeBackend) PowerSource() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.power == "" {
		return "ac"
	}
	return b.power
}
func (b *fakeBackend) Start() (Assertion, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.starts++
	if b.fail != nil {
		return nil, b.fail
	}
	if b.live != nil {
		panic("duplicate assertion")
	}
	b.live = &fakeAssertion{b: b, done: make(chan struct{})}
	return b.live, nil
}
func (a *fakeAssertion) Done() <-chan struct{} { return a.done }
func (a *fakeAssertion) Stop() {
	a.once.Do(func() {
		a.b.mu.Lock()
		a.b.stopped++
		a.b.live = nil
		a.b.mu.Unlock()
		close(a.done)
	})
}

// die ends the assertion as if the child crashed.
func (a *fakeAssertion) die() {
	a.once.Do(func() {
		a.b.mu.Lock()
		a.b.live = nil
		a.b.mu.Unlock()
		close(a.done)
	})
}

func (b *fakeBackend) asserting() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.live != nil
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func setup(t *testing.T) (*Manager, *fakeBackend, *clock, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "power.json")
	b := &fakeBackend{}
	m, err := Load(path, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)}
	m.now = c.now
	return m, b, c, path
}

// waitWatch gives the assertion watcher goroutine a moment to run.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met")
}

func TestOffByDefaultAndPersists(t *testing.T) {
	m, b, _, path := setup(t)
	if st := m.Status(); st.Enabled || st.State != "off" || !st.Supported {
		t.Fatalf("fresh status: %+v", st)
	}
	m.Acquire("v1")
	if b.starts != 0 {
		t.Fatal("viewer acquired an assertion while the preference is off")
	}
	if err := m.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if !b.asserting() {
		t.Fatal("enabling with a viewer present did not assert")
	}
	again, err := Load(path, b, nil)
	if err != nil || !again.Status().Enabled {
		t.Fatalf("preference did not survive a reload: %v %+v", err, again.Status())
	}
	if st := again.Status(); st.State != "waiting" {
		t.Fatalf("enabled, no viewers: %q, want waiting", st.State)
	}
	m.SetEnabled(false)
	if b.asserting() || m.Status().State != "off" {
		t.Fatal("disabling did not release immediately")
	}
}

func TestOneAssertionSharedAndReleasedAfterGrace(t *testing.T) {
	m, b, c, _ := setup(t)
	m.SetEnabled(true)
	m.Acquire("a")
	m.Acquire("b")
	if b.starts != 1 || !b.asserting() {
		t.Fatalf("two viewers: starts=%d asserting=%v", b.starts, b.asserting())
	}
	m.Release("a")
	c.advance(ReleaseGrace + time.Second)
	m.Tick()
	if !b.asserting() {
		t.Fatal("released while a viewer remained")
	}
	m.Release("b")
	if !b.asserting() || m.Status().State != "keeping_awake" {
		t.Fatal("released before the grace period")
	}
	c.advance(ReleaseGrace / 2)
	m.Tick()
	m.Acquire("a") // reconnect during grace cancels the release
	c.advance(ReleaseGrace)
	m.Tick()
	if !b.asserting() || b.starts != 1 {
		t.Fatalf("reconnect during grace: asserting=%v starts=%d", b.asserting(), b.starts)
	}
	m.Release("a")
	c.advance(ReleaseGrace + time.Second)
	m.Tick()
	if b.asserting() || b.stopped != 1 || m.Status().State != "waiting" {
		t.Fatalf("final release: asserting=%v stopped=%d state=%s", b.asserting(), b.stopped, m.Status().State)
	}
}

func TestLeaseExpiryAndDuplicateRelease(t *testing.T) {
	m, b, c, _ := setup(t)
	m.SetEnabled(true)
	m.Acquire("phone")
	c.advance(LeaseTTL - time.Second)
	m.Tick()
	if m.Status().Viewers != 1 {
		t.Fatal("lease expired early")
	}
	m.Acquire("phone") // renewal
	c.advance(LeaseTTL - time.Second)
	m.Tick()
	if m.Status().Viewers != 1 {
		t.Fatal("renewal did not extend the lease")
	}
	c.advance(2 * time.Second) // now past expiry with no renewal
	m.Tick()
	if m.Status().Viewers != 0 || !b.asserting() {
		t.Fatalf("after expiry: viewers=%d asserting=%v (grace should hold)", m.Status().Viewers, b.asserting())
	}
	c.advance(ReleaseGrace)
	m.Tick()
	if b.asserting() || b.stopped != 1 {
		t.Fatalf("stale lease did not release: stopped=%d", b.stopped)
	}
	m.Release("phone")
	m.Release("phone")
	m.Tick()
	if b.stopped != 1 || b.starts != 1 {
		t.Fatalf("duplicate release side effects: starts=%d stopped=%d", b.starts, b.stopped)
	}
}

func TestBackendFailureIsVisibleAndBounded(t *testing.T) {
	m, b, c, _ := setup(t)
	b.fail = errors.New("no caffeinate")
	m.SetEnabled(true)
	m.Acquire("v")
	st := m.Status()
	if st.State != "waiting" || !strings.Contains(st.Error, "no caffeinate") || st.Asserting {
		t.Fatalf("after first failure: %+v", st)
	}
	// Each renewal keeps the viewer present; the backoff climbs 1s, 2s,
	// 4s, 8s and the fifth failure gives up.
	for i := 0; i < 10; i++ {
		c.advance(30 * time.Second)
		m.Acquire("v")
		m.Tick()
	}
	if b.starts != maxFailures {
		t.Fatalf("retries not bounded: %d starts", b.starts)
	}
	if m.Status().State != "unavailable" {
		t.Fatalf("state after giving up: %s", m.Status().State)
	}
	// Fixing the backend and toggling tries again.
	b.fail = nil
	m.SetEnabled(true)
	if !b.asserting() || m.Status().State != "keeping_awake" {
		t.Fatalf("toggle after fix: %+v", m.Status())
	}
}

func TestUnexpectedExitRetries(t *testing.T) {
	m, b, c, _ := setup(t)
	m.SetEnabled(true)
	m.Acquire("v")
	first := b.live
	first.die()
	waitFor(t, func() bool { return strings.Contains(m.Status().Error, "unexpectedly") })
	if b.asserting() {
		t.Fatal("restarted before the backoff")
	}
	c.advance(2 * time.Second)
	m.Tick()
	if !b.asserting() || b.starts != 2 {
		t.Fatalf("after backoff: asserting=%v starts=%d", b.asserting(), b.starts)
	}
	m.Release("v")
	c.advance(ReleaseGrace + time.Second)
	m.Tick()
	if b.asserting() {
		t.Fatal("not released after the viewer left")
	}
}

func TestBatteryIsNotKeepingAwake(t *testing.T) {
	m, b, c, _ := setup(t)
	m.SetEnabled(true)
	m.Acquire("v")
	if m.Status().State != "keeping_awake" {
		t.Fatal(m.Status().State)
	}
	b.mu.Lock()
	b.power = "battery"
	b.mu.Unlock()
	c.advance(psCacheTTL + time.Second)
	if st := m.Status(); st.State != "on_battery" || !st.Asserting || st.Power != "battery" {
		t.Fatalf("on battery: %+v", st)
	}
}

func TestShutdownAndUnsupported(t *testing.T) {
	m, b, _, _ := setup(t)
	m.SetEnabled(true)
	m.Acquire("v")
	m.Shutdown()
	if b.asserting() || b.stopped != 1 {
		t.Fatal("shutdown left the assertion")
	}

	none, err := Load(filepath.Join(t.TempDir(), "power.json"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := none.SetEnabled(true); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("enable without backend: %v", err)
	}
	none.Acquire("v")
	if st := none.Status(); st.State != "unsupported" || st.Supported || st.Enabled {
		t.Fatalf("unsupported status: %+v", st)
	}
	var nilm *Manager
	nilm.Acquire("x")
	nilm.Release("x")
	nilm.Tick()
	nilm.Shutdown()
	if nilm.Status().State != "unsupported" {
		t.Fatal("nil manager status")
	}
}
