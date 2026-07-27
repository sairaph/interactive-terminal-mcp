package session

import "sync"

// subscriberBuffer is how many output chunks one viewer may fall behind before
// it is resynchronized instead of queued.
const subscriberBuffer = 64

// broadcaster fans PTY output out to attached human viewers.
//
// Viewers are strictly secondary to the emulator: a viewer that stops reading
// is dropped from the stream and told to resynchronize, never allowed to block
// the pump. An agent's tool call must not slow down because someone left an
// attached window scrolled back.
type broadcaster struct {
	mu      sync.Mutex
	next    int
	targets map[int]*subscriber
	closed  bool
}

type subscriber struct {
	channel chan []byte
	// stale marks a viewer that overflowed. The next successful send is
	// preceded by a nil chunk, which the viewer reads as "you missed output,
	// redraw from the current screen".
	stale bool
}

func newBroadcaster() *broadcaster {
	return &broadcaster{targets: map[int]*subscriber{}}
}

func (b *broadcaster) subscribe() (<-chan []byte, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		channel := make(chan []byte)
		close(channel)
		return channel, func() {}
	}
	id := b.next
	b.next++
	target := &subscriber{channel: make(chan []byte, subscriberBuffer)}
	b.targets[id] = target
	return target.channel, func() { b.unsubscribe(id) }
}

func (b *broadcaster) unsubscribe(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if target, ok := b.targets[id]; ok {
		delete(b.targets, id)
		close(target.channel)
	}
}

// publish copies a chunk to every viewer. The chunk belongs to the pump's read
// buffer and is reused immediately, so each viewer gets its own copy.
func (b *broadcaster) publish(chunk []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || len(b.targets) == 0 {
		return
	}
	for _, target := range b.targets {
		if target.stale {
			// Try to hand over the resync marker; keep waiting if still full.
			select {
			case target.channel <- nil:
				target.stale = false
			default:
				continue
			}
		}
		copied := make([]byte, len(chunk))
		copy(copied, chunk)
		select {
		case target.channel <- copied:
		default:
			target.stale = true
		}
	}
}

func (b *broadcaster) closeAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for id, target := range b.targets {
		delete(b.targets, id)
		close(target.channel)
	}
}
