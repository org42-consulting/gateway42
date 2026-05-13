package main

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// Async batched log writers. Replaces fire-and-forget `go db.Exec(...)` calls
// that, with the single-writer pool, used to back up and exhaust goroutines
// under load.
//
// Two queues feed two worker goroutines. Each worker:
//   - drains until either flushThreshold entries are buffered OR
//     flushInterval has elapsed since the first buffered entry
//   - writes the entire batch in one transaction
//
// On overflow, entries are dropped and a counter is incremented. Dropping
// a log line is preferable to OOM-ing under burst.

const (
	logQueueSize   = 1024
	flushInterval  = 500 * time.Millisecond
	flushThreshold = 100
)

// --- interaction logs ---

type interactionLogEntry struct {
	userID   int
	model    string
	prompt   string
	response string
	ts       string
}

var (
	chInteraction      = make(chan interactionLogEntry, logQueueSize)
	droppedInteraction atomic.Uint64
)

func enqueueInteractionLog(userID int, prompt, response, model string) {
	e := interactionLogEntry{
		userID:   userID,
		model:    model,
		prompt:   truncateInput(prompt),
		response: truncateInput(response),
		ts:       time.Now().UTC().Format(time.RFC3339),
	}
	select {
	case chInteraction <- e:
	default:
		droppedInteraction.Add(1)
		metricLogsDropped.WithLabelValues("interaction").Inc()
	}
}

func flushInteractionBatch(batch []interactionLogEntry) {
	if len(batch) == 0 {
		return
	}
	tx, err := db.Begin()
	if err != nil {
		slog.Error("interaction log tx", "err", err)
		return
	}
	stmt, err := tx.Prepare("INSERT INTO logs(user_id, model, prompt, response, ts) VALUES(?,?,?,?,?)")
	if err != nil {
		tx.Rollback()
		slog.Error("interaction log prepare", "err", err)
		return
	}
	for _, e := range batch {
		if _, err := stmt.Exec(e.userID, e.model, e.prompt, e.response, e.ts); err != nil {
			slog.Error("interaction log exec", "err", err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		slog.Error("interaction log commit", "err", err)
	}
}

// --- request logs ---

type requestLogEntry struct {
	ts       string
	method   string
	path     string
	clientIP string
	userName string
	status   int
}

var (
	chRequest      = make(chan requestLogEntry, logQueueSize)
	droppedRequest atomic.Uint64
)

func enqueueRequestLog(method, path, clientIP, userName string, status int) {
	e := requestLogEntry{
		ts:       time.Now().UTC().Format(time.RFC3339),
		method:   method,
		path:     path,
		clientIP: clientIP,
		userName: userName,
		status:   status,
	}
	select {
	case chRequest <- e:
	default:
		droppedRequest.Add(1)
		metricLogsDropped.WithLabelValues("request").Inc()
	}
}

func flushRequestBatch(batch []requestLogEntry) {
	if len(batch) == 0 {
		return
	}
	tx, err := db.Begin()
	if err != nil {
		slog.Error("request log tx", "err", err)
		return
	}
	stmt, err := tx.Prepare("INSERT INTO request_logs(ts, method, path, client_ip, user_name, status_code) VALUES(?,?,?,?,?,?)")
	if err != nil {
		tx.Rollback()
		slog.Error("request log prepare", "err", err)
		return
	}
	for _, e := range batch {
		if _, err := stmt.Exec(e.ts, e.method, e.path, e.clientIP, e.userName, e.status); err != nil {
			slog.Error("request log exec", "err", err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		slog.Error("request log commit", "err", err)
	}
}

// --- generic worker loop ---

func runInteractionWriter(ctx context.Context) {
	batch := make([]interactionLogEntry, 0, flushThreshold)
	timer := time.NewTimer(time.Hour) // initially long; reset when first item arrives
	timer.Stop()
	timerActive := false

	flush := func() {
		if len(batch) > 0 {
			flushInteractionBatch(batch)
			batch = batch[:0]
		}
		if timerActive {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timerActive = false
		}
	}

	for {
		select {
		case <-ctx.Done():
			// Drain remaining buffered entries best-effort.
			for {
				select {
				case e := <-chInteraction:
					batch = append(batch, e)
				default:
					flush()
					return
				}
			}
		case e := <-chInteraction:
			batch = append(batch, e)
			if !timerActive {
				timer.Reset(flushInterval)
				timerActive = true
			}
			if len(batch) >= flushThreshold {
				flush()
			}
		case <-timer.C:
			timerActive = false
			flush()
		}
	}
}

func runRequestWriter(ctx context.Context) {
	batch := make([]requestLogEntry, 0, flushThreshold)
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	timerActive := false

	flush := func() {
		if len(batch) > 0 {
			flushRequestBatch(batch)
			batch = batch[:0]
		}
		if timerActive {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timerActive = false
		}
	}

	for {
		select {
		case <-ctx.Done():
			for {
				select {
				case e := <-chRequest:
					batch = append(batch, e)
				default:
					flush()
					return
				}
			}
		case e := <-chRequest:
			batch = append(batch, e)
			if !timerActive {
				timer.Reset(flushInterval)
				timerActive = true
			}
			if len(batch) >= flushThreshold {
				flush()
			}
		case <-timer.C:
			timerActive = false
			flush()
		}
	}
}

// startLogWriters launches the two writer goroutines. They terminate when
// ctx is cancelled. The returned function blocks until both have drained.
func startLogWriters(ctx context.Context) (waitDone func()) {
	done := make(chan struct{}, 2)
	go func() { runInteractionWriter(ctx); done <- struct{}{} }()
	go func() { runRequestWriter(ctx); done <- struct{}{} }()

	// Periodic log of drops (every minute) so operators see backpressure.
	go func() {
		t := time.NewTicker(1 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if di := droppedInteraction.Swap(0); di > 0 {
					slog.Warn("dropped interaction logs in last minute", "count", di)
				}
				if dr := droppedRequest.Swap(0); dr > 0 {
					slog.Warn("dropped request logs in last minute", "count", dr)
				}
			}
		}
	}()

	return func() {
		<-done
		<-done
	}
}

