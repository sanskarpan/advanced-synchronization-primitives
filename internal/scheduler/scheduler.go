package scheduler

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// PrimitiveType represents the type of synchronization primitive
type PrimitiveType string

const (
	TypeRWLock       PrimitiveType = "RWLock"
	TypeFairRWLock   PrimitiveType = "FairRWLock"
	TypeSemaphore    PrimitiveType = "Semaphore"
	TypeMutex        PrimitiveType = "Mutex"
	TypeCondVar      PrimitiveType = "CondVar"
	TypeBarrier      PrimitiveType = "Barrier"
	TypeWaitGroup    PrimitiveType = "WaitGroup"
	TypeOnce         PrimitiveType = "Once"
	TypeSingleflight PrimitiveType = "Singleflight"
)

// GoroutineState represents the state of a goroutine
type GoroutineState string

const (
	StateRunning  GoroutineState = "Running"
	StateBlocked  GoroutineState = "Blocked"
	StateWaiting  GoroutineState = "Waiting"
	StateFinished GoroutineState = "Finished"
)

// maxGoroutines is the cap on registered goroutines to prevent unbounded growth.
const maxGoroutines = 10000

// PrimitiveInfo contains information about a synchronization primitive
type PrimitiveInfo struct {
	ID           string
	Type         PrimitiveType
	Name         string
	CreatedAt    time.Time
	BlockedCount int32
	Stats        interface{} // Type-specific stats
}

// GoroutineInfo contains information about a goroutine
type GoroutineInfo struct {
	ID         uint64
	Name       string
	State      GoroutineState
	StartedAt  time.Time
	FinishedAt time.Time
	BlockedOn  string // Primitive ID
	WaitTime   time.Duration
}

// Scheduler manages synchronization primitives and goroutines
type Scheduler struct {
	primitives map[string]*PrimitiveInfo
	goroutines map[uint64]*GoroutineInfo
	mu         sync.RWMutex

	// Metrics
	totalPrimitives atomic.Int64
	totalGoroutines atomic.Int64
	totalBlocks     atomic.Int64
	totalWaitTime   atomic.Int64

	// startTime records when the scheduler was created for uptime calculation.
	startTime time.Time

	// Events — written only by the eventWriter goroutine under eventsMu.
	events   []Event
	eventsMu sync.RWMutex
	// eventCh decouples callers (who hold mu) from the event-writer goroutine.
	// Sized at 256 so that a burst of events does not block callers.
	// If the channel is full, the emit() call drops the event (non-blocking).
	eventCh   chan Event
	maxEvents int

	// Update channel for real-time monitoring.
	// updateChan is never closed; consumers exit via done.
	updateChan chan *SchedulerUpdate
	// done is closed by Stop() to signal all goroutines to exit.
	done    chan struct{}
	running atomic.Bool
}

// Event represents a scheduler event
type Event struct {
	Timestamp   time.Time
	Type        string
	GoroutineID uint64
	PrimitiveID string
	Message     string
}

// SchedulerUpdate contains updates for monitoring
type SchedulerUpdate struct {
	Primitives map[string]*PrimitiveInfo
	Goroutines map[uint64]*GoroutineInfo
	Events     []Event
	Metrics    SchedulerMetrics
}

// SchedulerMetrics contains overall metrics
type SchedulerMetrics struct {
	TotalPrimitives   int64
	TotalGoroutines   int64
	ActiveGoroutines  int
	BlockedGoroutines int
	TotalBlocks       int64
	AvgWaitTime       time.Duration
	Uptime            time.Duration
}

// NewScheduler creates a new scheduler
func NewScheduler() *Scheduler {
	return &Scheduler{
		primitives: make(map[string]*PrimitiveInfo),
		goroutines: make(map[uint64]*GoroutineInfo),
		events:     make([]Event, 0),
		eventCh:    make(chan Event, 256),
		maxEvents:  1000,
		updateChan: make(chan *SchedulerUpdate, 100),
		done:       make(chan struct{}),
		startTime:  time.Now(),
	}
}

// Start starts the scheduler
func (s *Scheduler) Start() {
	s.running.Store(true)
	go s.eventWriter()
	go s.broadcastUpdates()
}

// Stop stops the scheduler. Safe to call once; subsequent calls are no-ops.
func (s *Scheduler) Stop() {
	if s.running.CompareAndSwap(true, false) {
		close(s.done)
	}
}

// Done returns a channel that is closed when Stop is called.
// Consumers (e.g. forwardSchedulerUpdates) use this to exit cleanly
// without relying on updateChan being closed.
func (s *Scheduler) Done() <-chan struct{} {
	return s.done
}

// RegisterPrimitive registers a new synchronization primitive
func (s *Scheduler) RegisterPrimitive(id string, ptype PrimitiveType, name string, stats interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()

	info := &PrimitiveInfo{
		ID:        id,
		Type:      ptype,
		Name:      name,
		CreatedAt: time.Now(),
		Stats:     stats,
	}

	s.primitives[id] = info
	s.totalPrimitives.Add(1)

	s.emit(Event{
		Timestamp:   time.Now(),
		Type:        "PrimitiveCreated",
		PrimitiveID: id,
		Message:     fmt.Sprintf("%s '%s' created", ptype, name),
	})
}

// UnregisterPrimitive unregisters a synchronization primitive
func (s *Scheduler) UnregisterPrimitive(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if info, exists := s.primitives[id]; exists {
		delete(s.primitives, id)
		s.totalPrimitives.Add(-1)

		s.emit(Event{
			Timestamp:   time.Now(),
			Type:        "PrimitiveDestroyed",
			PrimitiveID: id,
			Message:     fmt.Sprintf("%s '%s' destroyed", info.Type, info.Name),
		})
	}
}

// UpdatePrimitiveStats updates the stats for a primitive
func (s *Scheduler) UpdatePrimitiveStats(id string, stats interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if info, exists := s.primitives[id]; exists {
		info.Stats = stats
	}
}

// RegisterGoroutine registers a new goroutine
func (s *Scheduler) RegisterGoroutine(id uint64, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.goroutines) >= maxGoroutines {
		// Drop registration when cap is reached to prevent unbounded growth.
		return
	}

	info := &GoroutineInfo{
		ID:        id,
		Name:      name,
		State:     StateRunning,
		StartedAt: time.Now(),
	}

	s.goroutines[id] = info
	s.totalGoroutines.Add(1)

	s.emit(Event{
		Timestamp:   time.Now(),
		Type:        "GoroutineStarted",
		GoroutineID: id,
		Message:     fmt.Sprintf("Goroutine '%s' started", name),
	})
}

// UnregisterGoroutine marks a goroutine as finished and immediately removes
// it from the map (R5: prune finished goroutines immediately).
func (s *Scheduler) UnregisterGoroutine(id uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if info, exists := s.goroutines[id]; exists {
		s.emit(Event{
			Timestamp:   time.Now(),
			Type:        "GoroutineFinished",
			GoroutineID: id,
			Message:     fmt.Sprintf("Goroutine '%s' finished", info.Name),
		})
		delete(s.goroutines, id)
	}
}

// BlockGoroutine marks a goroutine as blocked on a primitive
func (s *Scheduler) BlockGoroutine(goroutineID uint64, primitiveID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if ginfo, exists := s.goroutines[goroutineID]; exists {
		ginfo.State = StateBlocked
		ginfo.BlockedOn = primitiveID

		if pinfo, pexists := s.primitives[primitiveID]; pexists {
			pinfo.BlockedCount++
		}

		s.totalBlocks.Add(1)

		s.emit(Event{
			Timestamp:   time.Now(),
			Type:        "GoroutineBlocked",
			GoroutineID: goroutineID,
			PrimitiveID: primitiveID,
			Message:     fmt.Sprintf("Goroutine '%s' blocked on '%s'", ginfo.Name, primitiveID),
		})
	}
}

// UnblockGoroutine marks a goroutine as unblocked
func (s *Scheduler) UnblockGoroutine(goroutineID uint64, waitTime time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if ginfo, exists := s.goroutines[goroutineID]; exists {
		primitiveID := ginfo.BlockedOn

		ginfo.State = StateRunning
		ginfo.BlockedOn = ""
		ginfo.WaitTime += waitTime

		if pinfo, pexists := s.primitives[primitiveID]; pexists {
			pinfo.BlockedCount--
		}

		s.totalWaitTime.Add(int64(waitTime))

		s.emit(Event{
			Timestamp:   time.Now(),
			Type:        "GoroutineUnblocked",
			GoroutineID: goroutineID,
			PrimitiveID: primitiveID,
			Message:     fmt.Sprintf("Goroutine '%s' unblocked (waited %v)", ginfo.Name, waitTime),
		})
	}
}

// GetPrimitives returns all registered primitives
func (s *Scheduler) GetPrimitives() map[string]*PrimitiveInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make(map[string]*PrimitiveInfo)
	for k, v := range s.primitives {
		infoCopy := *v
		result[k] = &infoCopy
	}

	return result
}

// GetGoroutines returns all registered goroutines
func (s *Scheduler) GetGoroutines() map[uint64]*GoroutineInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make(map[uint64]*GoroutineInfo)
	for k, v := range s.goroutines {
		infoCopy := *v
		result[k] = &infoCopy
	}

	return result
}

// GetEvents returns recent events
func (s *Scheduler) GetEvents(limit int) []Event {
	s.eventsMu.RLock()
	defer s.eventsMu.RUnlock()

	if limit <= 0 || limit > len(s.events) {
		limit = len(s.events)
	}

	start := len(s.events) - limit
	result := make([]Event, limit)
	copy(result, s.events[start:])

	return result
}

// GetMetrics returns scheduler metrics
func (s *Scheduler) GetMetrics() SchedulerMetrics {
	s.mu.RLock()
	defer s.mu.RUnlock()

	activeGoroutines := 0
	blockedGoroutines := 0

	for _, g := range s.goroutines {
		if g.State == StateRunning || g.State == StateBlocked {
			activeGoroutines++
		}
		if g.State == StateBlocked {
			blockedGoroutines++
		}
	}

	totalWait := s.totalWaitTime.Load()
	totalBlocks := s.totalBlocks.Load()
	avgWait := time.Duration(0)
	if totalBlocks > 0 {
		avgWait = time.Duration(totalWait / totalBlocks)
	}

	return SchedulerMetrics{
		TotalPrimitives:   s.totalPrimitives.Load(),
		TotalGoroutines:   s.totalGoroutines.Load(),
		ActiveGoroutines:  activeGoroutines,
		BlockedGoroutines: blockedGoroutines,
		TotalBlocks:       totalBlocks,
		AvgWaitTime:       avgWait,
		Uptime:            time.Since(s.startTime),
	}
}

// GetUpdateChannel returns the update channel for real-time monitoring
func (s *Scheduler) GetUpdateChannel() <-chan *SchedulerUpdate {
	return s.updateChan
}

// emit sends an event to the eventCh channel in a non-blocking manner.
// Must be called with mu held (or after mu is released is fine too; the
// decoupling means eventsMu is only acquired inside eventWriter).
// Drops the event if the channel is full.
func (s *Scheduler) emit(event Event) {
	select {
	case s.eventCh <- event:
	default:
		// Channel full; drop this event rather than blocking the caller.
	}
}

// eventWriter reads from eventCh and appends events to s.events under
// eventsMu only. This decouples mu from eventsMu so callers that hold mu
// can call emit() without the risk of a nested-lock deadlock.
// When done is closed, the writer drains any remaining events then exits.
func (s *Scheduler) eventWriter() {
	for {
		select {
		case event := <-s.eventCh:
			s.appendEvent(event)
		case <-s.done:
			// Drain remaining events
			for {
				select {
				case event := <-s.eventCh:
					s.appendEvent(event)
				default:
					return
				}
			}
		}
	}
}

// appendEvent appends one event to the log under eventsMu.
func (s *Scheduler) appendEvent(event Event) {
	s.eventsMu.Lock()
	defer s.eventsMu.Unlock()

	s.events = append(s.events, event)

	// Keep only maxEvents
	if len(s.events) > s.maxEvents {
		s.events = s.events[len(s.events)-s.maxEvents:]
	}
}

// broadcastUpdates periodically broadcasts updates until Stop() is called.
// It never closes updateChan; consumers exit via Done().
func (s *Scheduler) broadcastUpdates() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
		}

		update := &SchedulerUpdate{
			Primitives: s.GetPrimitives(),
			Goroutines: s.GetGoroutines(),
			Events:     s.GetEvents(100),
			Metrics:    s.GetMetrics(),
		}

		select {
		case s.updateChan <- update:
		case <-s.done:
			return
		default:
			// Channel full, skip this update
		}
	}
}
