package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"
)

// A session hub: one held attachment to a dtach session, a bounded buffer of its recent
// output, and the set of clients currently watching it.
//
//	dtach socket ──(one long-lived `wt <name>` held here)──▶ ring ──▶ N WebSocket clients
//
// Why this exists at all: dtach keeps no screen buffer, so an attaching client saw a blank
// screen until something wrote (measured: 64 bytes, none of them the output the previous
// client had seen). Nothing downstream of dtach can fix that — the bytes are simply gone —
// so wtd holds the attachment itself and remembers the tail.
//
// What it deliberately does NOT do: own the session. dtach still does. `bin/wt` is still the
// start command, so `dtach -A` still creates-or-attaches and session logic has exactly one
// implementation. Restarting wtd drops clients and loses buffers, and every session keeps
// running — that property is the reason dtach is still here and must not regress.
//
// Only *named* connections (`?arg=`) get a hub. A connection with no arg lands on `bin/wt`'s
// interactive picker, and wtd cannot know which session it ends up in — there is no key to
// buffer under, and sharing one picker between two clients would interleave their
// keystrokes. Those connections keep their own private pty, exactly as before.
type hub struct {
	// key is the full argv, so two clients only share a pty when they asked for the same
	// thing. name is argv[0], used to correlate with a dtach socket for `attached`.
	// Neither is validated here: session-name policy lives in validateAttachName
	// (api/ws-protocol.md section 8), and a name it rejects yields a hub whose pty is running a
	// plain shell — still shared, still named, which no session ever matches.
	key  string
	name string
	mgr  *hubs

	// notice is shown to every client joining this hub, before replay, when the session name it
	// asked for could not be used. Per-hub rather than per-client because the reason is a property
	// of the name, and every client on this key asked for the same one.
	notice string

	cmd    *exec.Cmd
	ptmx   *os.File
	waited chan error
	done   chan struct{} // closed once teardown has finished

	mu         sync.Mutex
	ring       *ring
	subs       map[*subscriber]struct{}
	cols, rows int
	closed     bool
	idleSince  time.Time // when the last client left; zero while any client is attached

	kicking atomic.Bool
}

// subscriber is one client's view of a hub. Frames arrive pre-framed with the OUTPUT opcode
// and are shared read-only between subscribers, so a fan-out costs one allocation per pty
// read regardless of how many clients are watching.
type subscriber struct {
	done   chan struct{} // closed when the hub drops this subscriber
	reason string        // why, valid once done is closed
	// closeCode is the WebSocket status the client is closed with. It is set here rather
	// than inferred in the ws layer because only the hub knows which of its three exits
	// this was, and api/ws-protocol.md section 13 gives each one a different code and a
	// different instruction to the client.
	closeCode websocket.StatusCode

	// mu guards queue, bytes and tailOwned. Deliberately not the hub's mutex: the writer
	// drains under this one, so a slow client's socket can never block the pty pump.
	mu sync.Mutex

	// queue is this subscriber's pending output, oldest first.
	//
	// A slice and not a channel, because the tail has to be reachable. Slots are the wrong
	// unit for a limit: a read(2) on a pty returns what is there, not the ptyReadChunk
	// ceiling, so no slot count can make bytes the binding limit — a frame ceiling is a byte
	// ceiling over an unbounded denominator. With the tail reachable, output for a client
	// that is already behind merges into its last pending frame instead of taking another
	// slot, which leaves the byte budget as the only limit (#66).
	//
	// Merging also raises the drain rate of the client that was too slow, which is the part
	// that actually breaks the reconnect loop: the iOS client feeds SwiftTerm once per frame
	// on the main thread, so fewer, larger frames are parsed and painted faster. It is a
	// cause-fix, not a threshold-fix.
	//
	// api/ws-protocol.md section 6 permits this — coalescing consecutive pty reads into one
	// OUTPUT frame is allowed provided bytes are not reordered and nothing is held on a
	// timer, and nothing here is. Note what that does *not* claim: merging fires whenever a
	// frame is pending at all, which during a flood is most reads, so a client that is one
	// scheduling quantum behind does see coalesced frames. Never larger ones than the pump
	// itself emits, though, which is the property clients actually depend on.
	queue [][]byte

	// bytes is the backlog measure: exactly the total length of queue.
	//
	// It measures len and not cap, so it undercounts residency — an owned tail can hold up
	// to twice what it is charged for. Bounded, deliberately, and the reason the merge below
	// sizes its first copy to the content rather than to ptyReadChunk.
	bytes int64

	// tailOwned records whether queue's last frame is a copy this subscriber made, and so may
	// be appended to in place. Two things depend on it, and the second is the load-bearing
	// one: without it each merge would re-copy the whole tail, so coalescing a megabyte would
	// cost a gigabyte of memmove — and frames are shared read-only with every other
	// subscriber and with the replay ring, so appending into one that is not ours would
	// corrupt what another client is about to be sent.
	tailOwned bool

	// wake nudges the writer that queue is non-empty. Buffered(1) and carrying no data, so a
	// burst costs one send and the writer drains whatever it finds; a token left over from a
	// drained burst only ever causes one spurious, harmless wakeup.
	wake chan struct{}

	// onDrop unblocks a client that is not draining. Closing done is enough for a
	// subscriber whose writer is idle, but the reason we drop a backlogged client is
	// precisely that its writer is stuck in conn.Write — only closing the connection
	// releases it, and that lives in the ws layer.
	onDrop func()
}

// offer queues one pre-framed OUTPUT message, merging it into the tail when the client is
// behind. It reports false only when the byte budget is exhausted — the single condition
// that drops a client.
//
// The backlog and frame count are returned rather than read back afterwards, because the
// writer drains concurrently: by the time a caller could ask, the numbers would understate
// what the refusal saw, and being able to trust them is the whole point of logging them.
func (s *subscriber) offer(frame []byte) (backlog int64, pending int, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// frame[0] is the OUTPUT opcode. A merge keeps the tail's copy and drops this one, so the
	// result is one well-formed frame rather than two concatenated with an opcode byte
	// stranded in the middle of the output stream.
	body := frame[1:]

	last := len(s.queue) - 1
	// Capped at what the pump itself emits, which is ptyReadChunk+1 — a read of ptyReadChunk
	// bytes plus the opcode byte. Clients depend on the upper bound, not on a particular size
	// (api/ws-protocol.md section 6), so a merged frame is never a shape a busy session would
	// not also produce.
	//
	// The +1 is load-bearing, not tidiness. Capping at ptyReadChunk instead refuses to merge
	// two half-chunk frames — 8193+8192 exceeds it by exactly one byte — so every frame takes
	// its own entry and the queue reaches 512 of them at the budget, twice what it should.
	// Measured, before and after.
	//
	// The resulting entry bound is maxSubBacklogBytes/ptyReadChunk, but approximately: an
	// entry closes as soon as the next *whole* body will not fit, so it can end up to one
	// body short. Measured worst case across 1-byte to full-chunk reads, and against
	// alternating tiny and near-full reads, is 260 entries against a bound of 256. Memory is
	// bounded by s.bytes regardless — that is the guarantee; this is a consequence of it, and
	// stating it more precisely than it is measured is how #66 happened.
	merge := last >= 0 && len(s.queue[last])+len(body) <= ptyReadChunk+1

	add := int64(len(frame))
	if merge {
		add = int64(len(body))
	}
	if s.bytes+add > maxSubBacklogBytes {
		return s.bytes, len(s.queue), false
	}

	switch {
	case !merge:
		s.queue = append(s.queue, frame)
		s.tailOwned = false
	case s.tailOwned:
		s.queue[last] = append(s.queue[last], body...)
	default:
		// Sized to the content, not to ptyReadChunk. Preallocating a full chunk here charges
		// 16 KiB of residency for what is usually a few hundred bytes, and the common case is
		// exactly that: merging fires the moment one frame is pending, so a client a single
		// scheduling quantum behind takes this branch constantly and catches straight up.
		// append's geometric growth covers the client that stays behind.
		merged := make([]byte, 0, len(s.queue[last])+len(body))
		merged = append(merged, s.queue[last]...)
		s.queue[last] = append(merged, body...)
		s.tailOwned = true
	}
	s.bytes += add

	select {
	case s.wake <- struct{}{}:
	default:
	}
	return s.bytes, len(s.queue), true
}

// pop takes the oldest pending frame. The second result is false when nothing is pending.
func (s *subscriber) pop() ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.queue) == 0 {
		return nil, false
	}
	frame := s.queue[0]
	s.queue[0] = nil // let the frame be collected before the slice header catches up
	s.queue = s.queue[1:]
	s.bytes -= int64(len(frame))
	if len(s.queue) == 0 {
		// Release the backing array, and with it any claim that the tail is ours to append to.
		s.queue = nil
		s.tailOwned = false
	}
	return frame, true
}

// newSubscriber exists so wake is never nil. A nil channel makes offer's non-blocking send
// fall through its default and the writer block on <-wake forever: output silently stops with
// nothing to point at the cause.
func newSubscriber(onDrop func()) *subscriber {
	return &subscriber{
		done:   make(chan struct{}),
		wake:   make(chan struct{}, 1),
		onDrop: onDrop,
	}
}

// backlog is the bytes this subscriber has pending. Tests only — broadcast uses what offer
// returns, because reading back afterwards races the writer.
func (s *subscriber) backlog() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes
}

// pending is the number of frames waiting, for diagnostics and tests.
func (s *subscriber) pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}

const (
	// Replay budget per session. 256 KiB is several screens of history — enough that a
	// reconnecting client sees what it missed — at a cost that does not matter at the scale
	// this runs at: 20 *idle* sessions is ~5 MiB of ring, against 20 dtach masters those
	// sessions already cost. That figure is rings only; a connected client can additionally
	// hold up to maxSubBacklogBytes while it is behind. Settable per install via
	// WT_REPLAY_BYTES; 0 disables replay while leaving hubs in place.
	defaultReplayBytes = 256 * 1024

	// Cap on hubs held for sessions nobody is watching, enforced when a new hub is created —
	// see evictWarmLocked. Not exact by design: a hub whose last client leaves after that
	// check can sit above the cap until the next creation, which is the only event that could
	// grow the set anyway.
	defaultMaxWarmHubs = 32

	// How far behind a client may fall before it is disconnected rather than allowed to stall
	// the session for everyone else.
	//
	// This is a memory guard, not the liveness mechanism — keepAlive's ping already reaps a
	// client that has genuinely vanished. So the budget is set generously: a burst of output
	// that a client is briefly behind on is normal (`cat` of a large file, a build starting),
	// and disconnecting over it would be a visible glitch for no reason. Only a client that
	// has stopped draining entirely gets this far, and it is better served by reconnecting
	// and replaying the tail than by holding megabytes of stale output.
	//
	// It is also the *only* limit, which it was not always: a 512-slot frame channel used to
	// sit in front of it and fill first, at around 4% of this budget, because a flood of
	// small pty reads exhausts slots long before bytes (#66). subscriber.queue merges into its
	// tail instead of taking another slot, so this is the number that decides.
	maxSubBacklogBytes = 4 << 20

	// Delays for the one-time redraw kick, mirroring the iOS client's proven timings. The
	// SIGWINCH has to land after bin/wt has exec'd dtach and dtach has attached; earlier and
	// it goes to the shell script and is lost.
	kickDelay  = 400 * time.Millisecond
	kickSettle = 150 * time.Millisecond
)

var errHubClosed = errors.New("hub is closing")

// hubs is the set of live hubs, keyed by argv.
type hubs struct {
	// build turns a connection's argv into the command to run. A function rather than a start
	// command string because what a named connection runs is now a decision — attach to that
	// session, or fall back to a shell — and that decision belongs in exactly one place. See
	// server.terminalCommand.
	build       func(args []string) (terminalCmd, error)
	replayBytes int
	maxWarm     int // cap on hubs with no clients; the least recently idle is evicted

	mu      sync.Mutex
	m       map[string]*hub
	closing bool
	// reaping counts teardowns in flight, including the ones evictWarmLocked spawns. Without
	// it closeAll is not a barrier: an evicted hub is already out of the map, so closeAll
	// never sees it, and the process can exit with its escalation ladder half-run.
	reaping sync.WaitGroup
}

func newHubs(build func(args []string) (terminalCmd, error), replayBytes, maxWarm int) *hubs {
	return &hubs{
		build:       build,
		replayBytes: replayBytes,
		maxWarm:     maxWarm,
		m:           map[string]*hub{},
	}
}

// join attaches a client to the hub for args, creating it if this is the first client, and
// returns the history to replay before live output starts.
func (m *hubs) join(args []string, cols, rows int, onDrop func()) (*hub, *subscriber, []byte, error) {
	// Two attempts: a hub can be reaped (its session exited, or it was evicted) between
	// being found in the map and being subscribed to. One retry turns that race into a
	// fresh hub rather than a connection the client has to notice and redial.
	for attempt := 0; attempt < 2; attempt++ {
		h, err := m.getOrCreate(args, cols, rows)
		if err != nil {
			return nil, nil, nil, err
		}
		sub, replay, err := h.subscribe(onDrop)
		if err == nil {
			return h, sub, replay, nil
		}
		m.forget(h)
	}
	return nil, nil, nil, errors.New("session hub exited immediately on attach, twice")
}

func (m *hubs) getOrCreate(args []string, cols, rows int) (*hub, error) {
	key := hubKey(args)

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closing {
		// A hijacked WebSocket is invisible to http.Server.Shutdown, so a handler still in
		// its handshake can arrive here after closeAll has emptied the map. Spawning then
		// would leave a dtach client nothing ever reaps.
		return nil, errors.New("server is shutting down")
	}
	if h, ok := m.m[key]; ok {
		return h, nil
	}

	h, err := m.spawn(key, args, cols, rows)
	if err != nil {
		return nil, err
	}
	m.m[key] = h
	m.evictWarmLocked()
	return h, nil
}

// spawn starts the held start command on a pty. Called with m.mu held: a fork+exec is
// milliseconds and happens once per session rather than once per connection, which is
// cheaper than the bookkeeping a half-constructed hub in the map would need.
func (m *hubs) spawn(key string, args []string, cols, rows int) (*hub, error) {
	// The command is built rather than assembled here, so a named connection resolves its session
	// exactly as the private path and the JSON API do. exec.Command, not exec.CommandContext, for
	// the same reason as the private-pty path: this process outlives the request that created it by
	// design, so its lifecycle is explicit rather than tied to any context.
	tc, err := m.build(args)
	if err != nil {
		return nil, fmt.Errorf("%w: build command for %q: %w", errSpawnFailed, args[0], err)
	}
	cmd := tc.cmd

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		// errSpawnFailed so the joining client gets the published 1011 rather than an abrupt
		// drop. The failure happens here, inside join, before a subscriber exists — which is
		// why the close is sent by handleWS on the way out rather than through the hub's own
		// broadcast machinery, which has nobody to broadcast to yet.
		return nil, fmt.Errorf("%w: start %s: %w", errSpawnFailed, tc.label, err)
	}

	h := &hub{
		key:    key,
		name:   args[0],
		notice: tc.notice,
		mgr:    m,
		cmd:    cmd,
		ptmx:   ptmx,
		waited: make(chan error, 1),
		done:   make(chan struct{}),
		ring:   newRing(m.replayBytes),
		subs:   map[*subscriber]struct{}{},
		cols:   cols,
		rows:   rows,
	}
	// One Wait for the process's lifetime, started now so terminate() can wait on it with a
	// bound. cmd.Wait must be called exactly once.
	go func() { h.waited <- cmd.Wait() }()
	go h.pump()

	return h, nil
}

// hubKey joins on NUL, which cannot collide because filterArgs has already dropped every
// value containing one.
//
// The separator is only unambiguous because of that filter, and it was not always: URL
// decoding puts a real NUL into a value long before exec rejects it, so ?arg=a%00b once
// produced the same key as ?arg=a&arg=b and the two connections joined one hub — same pty,
// same ring, interleaved input. Do not weaken filterArgs without changing this.
func hubKey(args []string) string { return strings.Join(args, "\x00") }

// evictWarmLocked bounds the number of hubs held for sessions nobody is watching.
//
// A hub costs one dtach client process and up to the replay budget in memory, and it is held
// until its session dies — which tracks reality, since every session already costs a dtach
// master. What it does not bound on its own is an unauthenticated peer connecting once per
// invented name and walking away, so the least-recently-idle warm hub is released past the
// cap. Hubs with clients are never evicted; those are bounded by open connections, as before.
func (m *hubs) evictWarmLocked() {
	type warm struct {
		h    *hub
		idle time.Time
	}
	var idle []warm
	for _, h := range m.m {
		h.mu.Lock()
		// A zero idleSince means "created, not yet subscribed to" — the hub the caller is
		// about to use. Excluded from the count as well as from eviction, or the cap would
		// trigger one hub early and settle at maxWarm+1.
		if len(h.subs) == 0 && !h.closed && !h.idleSince.IsZero() {
			idle = append(idle, warm{h, h.idleSince})
		}
		h.mu.Unlock()
	}
	if len(idle) <= m.maxWarm {
		return
	}

	// Only the excess, oldest first.
	for len(idle) > m.maxWarm {
		oldest := -1
		for i := range idle {
			if oldest < 0 || idle[i].idle.Before(idle[oldest].idle) {
				oldest = i
			}
		}
		if oldest < 0 {
			return // nothing evictable
		}
		victim := idle[oldest].h
		idle = append(idle[:oldest], idle[oldest+1:]...)
		delete(m.m, victim.key)
		// Teardown blocks on the signal ladder, so it must not happen under m.mu — but it
		// must still be waited for by closeAll, hence the counter.
		m.reaping.Add(1)
		go func() {
			defer m.reaping.Done()
			victim.reap(websocket.StatusGoingAway, "evicted: more warm hubs than the cap allows")
		}()
	}
}

// forget removes h from the map if it is still the hub registered under its key.
func (m *hubs) forget(h *hub) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.m[h.key]; ok && cur == h {
		delete(m.m, h.key)
	}
}

// hubStat is what the sessions API needs from a hub: how many clients are watching, and
// which process group holds wtd's own dtach client so it can be told apart from an external
// attach. See listSessions.
type hubStat struct {
	// Derived with len(), never incremented and decremented, and attachedTo depends on that:
	// it sums this with the external-client count and treats "> 0" as attached, which is
	// equivalent to the old signal-by-signal check only while this cannot go negative. A
	// counter that underflowed would silently report a watched session as idle.
	clients int
	// pgids holds every held attachment for this session name, not just one. Two hubs can
	// share a name because the key is the full argv while the name is argv[0], and bin/wt
	// reads only $1 — so `?arg=foo` and `?arg=foo&arg=x` are two hubs on one socket. Keeping
	// a single pgid made the second hub's own dtach client look like an external viewer, and
	// the session then read as attached forever: exactly the bug attachedTo exists to kill.
	pgids map[int]struct{}
}

// stats reports per session name. Names are argv[0], so a hub for a multi-arg deep link is
// reported under its first arg — the same correlation bin/wt makes.
func (m *hubs) stats() map[string]hubStat {
	m.mu.Lock()
	hs := make([]*hub, 0, len(m.m))
	for _, h := range m.m {
		hs = append(hs, h)
	}
	m.mu.Unlock()

	out := make(map[string]hubStat, len(hs))
	for _, h := range hs {
		h.mu.Lock()
		clients := len(h.subs)
		h.mu.Unlock()

		st, ok := out[h.name]
		if !ok {
			st.pgids = map[int]struct{}{}
		}
		st.clients += clients
		if h.cmd.Process != nil {
			// pty.Start puts the child in a new session, so its pgid equals its pid — and
			// bin/wt *execs* dtach, so this is the dtach client's own pid.
			st.pgids[h.cmd.Process.Pid] = struct{}{}
		}
		out[h.name] = st
	}
	return out
}

// closeAll tears down every hub, used on shutdown so dtach clients detach rather than being
// orphaned into a state where the sessions look permanently attached.
func (m *hubs) closeAll() {
	m.mu.Lock()
	hs := make([]*hub, 0, len(m.m))
	for _, h := range m.m {
		hs = append(hs, h)
	}
	m.m = map[string]*hub{}
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, h := range hs {
		wg.Add(1)
		go func(h *hub) {
			defer wg.Done()
			h.reap(websocket.StatusGoingAway, "server shutting down")
		}(h)
	}
	wg.Wait()
}

// subscribe registers a client and returns the history to send it first.
//
// The snapshot and the registration happen under one mutex, and the pty pump appends to the
// ring and fans out to subscribers under that same mutex. That is what makes the seam exact:
// a client registered at time T is sent every byte written before T as replay and every byte
// written after T as live output, with nothing counted twice and nothing missed. Any design
// where the snapshot and the registration are separate steps has a window, and the window is
// invisible until a client sees a duplicated or truncated prompt.
func (h *hub) subscribe(onDrop func()) (*subscriber, []byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed {
		return nil, nil, errHubClosed
	}
	sub := newSubscriber(onDrop)
	h.subs[sub] = struct{}{}
	h.idleSince = time.Time{}

	replay := h.ring.snapshot()
	// Nothing to replay means this client would be looking at a blank screen, so force a
	// repaint. That covers the first client of a fresh hub and — the case that made this
	// trigger better than kicking once at spawn — every attach when replay is switched off
	// with WT_REPLAY_BYTES=0, where the browser page has no kick of its own any more.
	if len(replay) == 0 {
		go h.kick()
	}
	return sub, replay, nil
}

// unsubscribe drops a client. The hub stays: holding the attachment while nobody is watching
// is what makes the *next* attach show context instead of a blank screen, which is the whole
// point of the ticket. It is released when its session exits, when the cap evicts it, or
// when wtd stops.
func (h *hub) unsubscribe(sub *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[sub]; !ok {
		return
	}
	delete(h.subs, sub)
	if len(h.subs) == 0 {
		h.idleSince = time.Now()
	}
}

// pump moves pty output into the ring and out to every subscriber.
func (h *hub) pump() {
	buf := make([]byte, ptyReadChunk)
	for {
		n, err := h.ptmx.Read(buf)
		if n > 0 {
			// Framed once, shared read-only by every subscriber and by the ring: one
			// allocation per pty read no matter how many clients are attached. Byte 0 is
			// the OUTPUT opcode so a subscriber can write the slice straight to its socket.
			frame := make([]byte, n+1)
			frame[0] = opOutput
			copy(frame[1:], buf[:n])
			h.broadcast(frame)
		}
		if err != nil {
			// EIO is how a pty reports that the child closed the other end — here that
			// means the held dtach client exited: the session was killed, its shell
			// exited, or a client sent Ctrl-\ and detached it.
			// Section 13: the start command exiting is a normal closure, and a client
			// should not silently reconnect into a session that has gone.
			h.reap(websocket.StatusNormalClosure, fmt.Sprintf("start command ended (%v)", err))
			return
		}
	}
}

func (h *hub) broadcast(frame []byte) {
	// Collected under the lock, logged after it. This runs on the pty pump, so a journald
	// write here stalls output for every client on the session — and the case that produces
	// these lines is a client reconnect-looping, which would make it a repeating stall.
	var dropped []string
	defer func() {
		for _, line := range dropped {
			log.Print(line)
		}
	}()

	h.mu.Lock()
	defer h.mu.Unlock()

	h.ring.write(frame[1:])

	for sub := range h.subs {
		backlog, pending, ok := sub.offer(frame)
		if ok {
			continue
		}
		// Dropping this client is deliberate: the alternative is blocking the pty pump,
		// which would stall the session for every other client and apply backpressure all
		// the way into the shell. A subscriber therefore either gets a contiguous stream or
		// gets closed — never a stream with a hole in it.
		//
		// Logged because the client is told and the operator was not (#65). The numbers are
		// the point: this used to fire at a few hundred KiB because a frame channel filled
		// first, and the only way to know that was to print them (#66).
		dropped = append(dropped, fmt.Sprintf(
			"wtd: hub %q dropping a client: output backlog %d of %d bytes in %d frame(s)",
			h.name, backlog, int64(maxSubBacklogBytes), pending))
		sub.reason = "output backlog exceeded"
		// 1013: the session is fine and the buffer will restore context, so this client
		// should come straight back rather than treat the close as final.
		sub.closeCode = websocket.StatusTryAgainLater
		close(sub.done)
		delete(h.subs, sub)
		if sub.onDrop != nil {
			sub.onDrop()
		}
	}
	if len(h.subs) == 0 && h.idleSince.IsZero() {
		h.idleSince = time.Now()
	}
}

// input relays a client's keystrokes. Every client writes to the same pty, which is
// screen -x semantics and what dtach itself does with two attached clients.
func (h *hub) input(p []byte) error {
	if _, err := h.ptmx.Write(p); err != nil {
		return fmt.Errorf("pty write: %w", err)
	}
	return nil
}

// resize applies a client's window size. Last writer wins, matching dtach's own behavior
// with multiple clients, and applied per frame with no coalescing — api/ws-protocol.md
// section 7 requires that, and the redraw kick depends on it.
func (h *hub) resize(cols, rows int) error {
	return h.setSize(cols, rows, true)
}

// setSize is the only path to TIOCSWINSZ, and it holds h.mu across the ioctl. Two reasons,
// both of which bit:
//
//   - Use-after-close. reap sets closed under h.mu and closes h.ptmx *after* releasing it,
//     and creack/pty's ioctl helper passes os.File.Fd() straight to the syscall — the raw
//     descriptor, with none of the reference counting that makes os.File.Write return
//     ErrClosed (its SyscallConn variant is "NOTE: Unused" upstream). So a resize racing
//     teardown can hand TIOCSWINSZ a number the kernel has already reassigned, silently
//     resizing an unrelated terminal. Checking closed under the same lock that sets it
//     closes that window.
//   - Ordering. Updating the fields under the lock and then leaving the ioctl unsynchronized
//     let two resizes apply in one order and land in the other, leaving h.cols/h.rows
//     permanently disagreeing with the pty.
//
// An ioctl on a pty master does not block, so holding the lock across it costs nothing.
func (h *hub) setSize(cols, rows int, record bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil // teardown won; there is no terminal left to resize
	}
	if record {
		h.cols, h.rows = cols, rows
	}
	return pty.Setsize(h.ptmx, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

// kick forces one repaint shortly after the hub attaches, so the ring starts with a screen
// in it rather than nothing.
//
// This is the surviving half of what used to be a two-sided workaround. dtach's own
// `-r winch` (bin/wt:26) sends a bare SIGWINCH on attach, which was not enough on its own —
// that is exactly why the iOS client learned to jiggle the size, and why the browser page
// did too. Doing it here instead means it happens once per session rather than once per
// client, in one repo rather than two, and every client benefits including builds that were
// installed before this shipped.
func (h *hub) kick() {
	// One kick at a time. Two clients joining a fresh hub together would otherwise stack
	// jiggles on the same pty for no benefit.
	if !h.kicking.CompareAndSwap(false, true) {
		return
	}
	defer h.kicking.Store(false)

	select {
	case <-h.done:
		return
	case <-time.After(kickDelay):
	}

	h.mu.Lock()
	cols, rows := h.cols, h.rows
	h.mu.Unlock()
	if rows <= 1 {
		return // nothing to jiggle without going below a 1-row terminal
	}

	// A real size change, not a bare SIGWINCH: programs that compare against the previous
	// size ignore a no-op resize, which is the trap the two-sided hack was working around.
	// record=false so the jiggle never becomes the hub's remembered size.
	if err := h.setSize(cols, rows-1, false); err != nil {
		return
	}

	select {
	case <-h.done:
		return
	case <-time.After(kickSettle):
	}

	// Re-read rather than reuse: a client may have resized during the settle window, and its
	// size must win rather than being reverted to the pre-kick one.
	h.mu.Lock()
	cols, rows = h.cols, h.rows
	h.mu.Unlock()
	_ = h.setSize(cols, rows, false)
}

// reap tears the hub down: drops every client, hangs up the pty and ends the process group.
//
// Ordering is load-bearing, the same way it is in the private-pty path. `closed` is set
// first so a client racing to subscribe is told to retry into a fresh hub instead of
// attaching to a dying one. The map entry goes next, so no new client can find it. Only then
// does the pty close and the signal ladder run — neither may happen under a lock, since
// terminate() blocks for up to the whole escalation.
func (h *hub) reap(code websocket.StatusCode, reason string) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		<-h.done // another caller is tearing it down; do not return before it is gone
		return
	}
	h.closed = true
	subs := make([]*subscriber, 0, len(h.subs))
	for sub := range h.subs {
		subs = append(subs, sub)
	}
	h.subs = map[*subscriber]struct{}{}
	h.mu.Unlock()

	h.mgr.forget(h)

	for _, sub := range subs {
		sub.reason = reason
		sub.closeCode = code
		close(sub.done)
	}

	_ = h.ptmx.Close()
	// SIGHUP first, which for the real start command ends the dtach *client* — detaching
	// and leaving the session running, the property everything here depends on.
	terminate(h.cmd, h.waited)
	close(h.done)

	if len(subs) > 0 {
		log.Printf("wtd: hub %q closed with %d client(s) attached: %s", h.name, len(subs), reason)
	}
}
