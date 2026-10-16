package storage

import (
	"context"
	"errors"
	"time"
)

const (
	// asyncQueueSize bounds the captures waiting for the writer; past it,
	// captures are dropped rather than slowing the DNS server down.
	asyncQueueSize = 10000
	// asyncBatch bounds the scripts sent in one pipeline.
	asyncBatch = 128
)

// Variables so tests can shorten them.
var (
	// asyncTimeout bounds one pipeline, so a stalled server cannot wedge the
	// writer forever.
	asyncTimeout = 5 * time.Second
	// drainTimeout bounds the flush on Close: with the server unreachable,
	// every batch would wait asyncTimeout and shutdown would take minutes.
	drainTimeout = 5 * time.Second
)

var errQueueFull = errors.New("capture queue is full")

type pendingCapture struct {
	hookID string
	it     *Interaction
}

// RecordAsync queues an interaction for the background writer. The caller
// must not use the interaction afterwards, and its Seq is never set.
func (m *RedisManager) RecordAsync(hookID string, interaction *Interaction) {
	if m.closed.Load() {
		return
	}
	select {
	case m.queue <- pendingCapture{hookID, interaction}:
	default:
		m.logFailure("queue capture", errQueueFull, "hook_id", hookID)
	}
}

// writeLoop sends queued captures in pipelines: one batch per round-trip
// under load, one capture at a time otherwise. A single writer keeps
// captures in arrival order.
func (m *RedisManager) writeLoop() {
	defer close(m.done)
	batch := make([]pendingCapture, 0, asyncBatch)
	for {
		select {
		case first := <-m.queue:
			ctx, cancel := context.WithTimeout(context.Background(), asyncTimeout)
			m.writeBatch(ctx, m.fill(append(batch[:0], first)))
			cancel()
		case <-m.stop:
			m.drain(batch)
			return
		}
	}
}

// drain flushes what was queued before Close, within drainTimeout.
func (m *RedisManager) drain(batch []pendingCapture) {
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	for len(m.queue) > 0 {
		if ctx.Err() != nil {
			m.logger.Warn("redis storage dropped queued captures on close", "count", len(m.queue))
			return
		}
		m.writeBatch(ctx, m.fill(batch[:0]))
	}
}

// fill tops up a batch with whatever is already queued, without waiting.
func (m *RedisManager) fill(batch []pendingCapture) []pendingCapture {
	for len(batch) < asyncBatch {
		select {
		case c := <-m.queue:
			batch = append(batch, c)
		default:
			return batch
		}
	}
	return batch
}

func (m *RedisManager) writeBatch(ctx context.Context, batch []pendingCapture) {
	args := make([][]any, 0, len(batch))
	pipe := m.client.Pipeline()
	for _, c := range batch {
		a, err := m.addArgs(c.hookID, c.it)
		if err != nil {
			m.logFailure("encode interaction", err, "hook_id", c.hookID)
			continue
		}
		args = append(args, a)
		addInteractionScript.EvalSha(ctx, pipe, nil, a...)
	}
	cmds, _ := pipe.Exec(ctx)
	reloaded := false
	for i, cmd := range cmds {
		err := cmd.Err()
		// A flushed script cache (restart, failover) fails every call after
		// the flush, so the replays keep arrival order: they are the tail of
		// the batch, run in order once the script is loaded again.
		if err != nil && isScriptError(err, "NOSCRIPT") {
			if !reloaded {
				reloaded = addInteractionScript.Load(ctx, m.client).Err() == nil
			}
			err = addInteractionScript.EvalSha(ctx, m.client, nil, args[i]...).Err()
		}
		if err != nil {
			m.logFailure("add interaction", err)
		}
	}
}
