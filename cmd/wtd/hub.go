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
	// Neither is validated here: session-name policy lives in bin/wt alone
	// (api/ws-protocol.md section 8), so a name bin/wt rejects simply yields a hub whose
	// pty is running the picker and which no session ever matches.
	key  string
	name string
	mgr  *hubs

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
	frames chan []byte
	done   chan struct{} // closed when the hub drops this subscriber
	reason string        // why, valid once done is closed
	// closeCode is the WebSocket status the client is closed with. It is set here rather
	// than inferred in the ws layer because only the hub knows which of its three exits
	// this was, and api/ws-protocol.md section 13 gives each one a different code and a
	// different instruction to the client.
	closeCode websocket.StatusCode

	// queued is the bytes handed to this subscriber and not yet written to its socket. It
	// is the backlog measure, and it is in bytes rather than frames for a reason found the
	// hard way: a 200 KiB burst arrives as ~60 small frames, so any frame-count limit low
	// enough to catch a stalled client also disconnects a healthy one that is a few
	// milliseconds behind.
	queued atomic.Int64

	// onDrop unblocks a client that is not draining. Closing done is enough for a
	// subscriber whose writer is idle, but the reason we drop a backlogged client is
	// precisely that its writer is stuck in conn.Write — only closing the connection
	// releases it, and that lives in the ws layer.
	onDrop func()
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
	maxSubBacklogBytes = 4 << 20

	// Channel depth is the secondary limit; the byte budget above is the one that decides.
	subQueueFrames = 512

	// Delays for the one-time redraw kick, mirroring the iOS client's proven timings. The
	// SIGWINCH has to land after bin/wt has exec'd dtach and dtach has attached; earlier and
	// it goes to the shell script and is lost.
	kickDelay  = 400 * time.Millisecond
	kickSettle = 150 * time.Millisecond
)

var errHubClosed = errors.New("hub is closing")

// hubs is the set of live hubs, keyed by argv.
type hubs struct {
	startCommand string
	replayBytes  int
	maxWarm      int // cap on hubs with no clients; the least recently idle is evicted

	mu      sync.Mutex
	m       map[string]*hub
	closing bool
	// reaping counts teardowns in flight, including the ones evictWarmLocked spawns. Without
	// it closeAll is not a barrier: an evicted hub is already out of the map, so closeAll
	// never sees it, and the process can exit with its escalation ladder half-run.
	reaping sync.WaitGroup
}

func newHubs(startCommand string, replayBytes, maxWarm int) *hubs {
	return &hubs{
		startCommand: startCommand,
		replayBytes:  replayBytes,
		maxWarm:      maxWarm,
		m:            map[string]*hub{},
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
	// exec.Command, not exec.CommandContext, for the same reason as the private-pty path:
	// this process outlives the request that created it by design, so its lifecycle is
	// explicit rather than tied to any context.
	cmd := exec.Command(m.startCommand, args...)
	cmd.Env = append(os.Environ(), "TERM="+defaultTerm)

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", m.startCommand, err)
	}

	h := &hub{
		key:    key,
		name:   args[0],
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

// hubKey joins on NUL, which cannot appear in an argv element and so cannot collide.
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
	sub := &subscriber{
		frames: make(chan []byte, subQueueFrames),
		done:   make(chan struct{}),
		onDrop: onDrop,
	}
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
	h.mu.Lock()
	defer h.mu.Unlock()

	h.ring.write(frame[1:])

	for sub := range h.subs {
		if sub.queued.Load()+int64(len(frame)) <= maxSubBacklogBytes {
			select {
			case sub.frames <- frame:
				sub.queued.Add(int64(len(frame)))
				continue
			default:
			}
		}
		// Dropping this client is deliberate: the alternative is blocking the pty pump,
		// which would stall the session for every other client and apply backpressure all
		// the way into the shell. A subscriber therefore either gets a contiguous stream or
		// gets closed — never a stream with a hole in it.
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
