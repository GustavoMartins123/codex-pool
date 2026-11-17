package main

import (
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"codex-pool-proxy/internal/sse"
)

// idleTimeoutReader wraps an io.ReadCloser and returns an error if no data
// is received for longer than the configured idle timeout. This prevents
// zombie SSE connections where the upstream stops sending data but never
// closes the TCP connection.
type idleTimeoutReader struct {
	rc        io.ReadCloser
	timeout   time.Duration
	timer     *time.Timer
	done      chan struct{}
	cancel    func() // cancel the request context
	timedOut  atomic.Bool
	closeOnce sync.Once
}

func newIdleTimeoutReader(rc io.ReadCloser, timeout time.Duration, cancel func()) *idleTimeoutReader {
	r := &idleTimeoutReader{
		rc:      rc,
		timeout: timeout,
		timer:   time.NewTimer(timeout),
		done:    make(chan struct{}),
		cancel:  cancel,
	}
	go r.watchdog()
	return r
}

func (r *idleTimeoutReader) watchdog() {
	select {
	case <-r.timer.C:
		// Idle timeout expired - cancel the request context which will
		// cause the Read to return with a context error.
		r.timedOut.Store(true)
		if r.cancel != nil {
			r.cancel()
		}
	case <-r.done:
		r.timer.Stop()
	}
}

func (r *idleTimeoutReader) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	if n > 0 {
		// Got data - reset the idle timer
		r.timer.Reset(r.timeout)
	}
	if err != nil {
		// Wrap context.Canceled with a more descriptive message when our idle timer fired
		if r.timedOut.Load() && (err.Error() == "context canceled" || err.Error() == "context deadline exceeded") {
			return n, fmt.Errorf("SSE stream idle for %v, closing", r.timeout)
		}
	}
	return n, err
}

func (r *idleTimeoutReader) Close() error {
	r.closeOnce.Do(func() {
		close(r.done)
		r.timer.Stop()
	})
	return r.rc.Close()
}

type limitedWriter struct {
	w io.Writer
	n int64
}

func (lw *limitedWriter) Write(p []byte) (int, error) {
	if lw.n <= 0 {
		return len(p), nil
	}
	if int64(len(p)) > lw.n {
		p = p[:lw.n]
	}
	n, err := lw.w.Write(p)
	lw.n -= int64(n)
	return len(p), err
}

type loggingReadCloser struct {
	io.ReadCloser
	onClose func()
}

func (rc *loggingReadCloser) Close() error {
	if rc.onClose != nil {
		rc.onClose()
	}
	return rc.ReadCloser.Close()
}

type flushWriter struct {
	w             http.ResponseWriter
	f             http.Flusher
	flushInterval time.Duration

	mu        sync.Mutex
	lastFlush time.Time
	pending   bool
	stopped   bool
	err       error
	timer     *time.Timer
	done      chan struct{}
	exited    chan struct{}
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if fw.err != nil {
		return 0, fw.err
	}
	if fw.stopped {
		return 0, io.ErrClosedPipe
	}

	n, err := fw.w.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		fw.err = err
		return n, err
	}
	if n == 0 {
		return n, nil
	}

	wasPending := fw.pending
	fw.pending = true
	if fw.flushInterval <= 0 || fw.lastFlush.IsZero() {
		return n, fw.flushLocked()
	}
	remaining := fw.flushInterval - time.Since(fw.lastFlush)
	if remaining <= 0 {
		return n, fw.flushLocked()
	}
	// Anchor the deadline to the last flush, not each new token.
	if !wasPending {
		if fw.timer == nil {
			fw.timer = time.NewTimer(remaining)
			fw.done = make(chan struct{})
			fw.exited = make(chan struct{})
			go fw.run()
		} else {
			fw.timer.Reset(remaining)
		}
	}
	return n, nil
}

func (fw *flushWriter) run() {
	defer close(fw.exited)
	for {
		select {
		case <-fw.done:
			return
		case <-fw.timer.C:
			fw.mu.Lock()
			if !fw.stopped {
				fw.flushLocked()
			}
			fw.mu.Unlock()
		}
	}
}

// flush shares the response lock with writes and the deadline timer.
func (fw *flushWriter) flush() error {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if fw.stopped {
		if fw.err != nil {
			return fw.err
		}
		return io.ErrClosedPipe
	}
	return fw.flushLocked()
}

func (fw *flushWriter) flushLocked() error {
	if fw.timer != nil {
		fw.timer.Stop()
	}
	if fw.err != nil || !fw.pending {
		return fw.err
	}
	fw.pending = false
	fw.err = flushHTTP(fw.f)
	if fw.flushInterval > 0 {
		fw.lastFlush = time.Now()
	}
	return fw.err
}

func flushHTTP(f http.Flusher) error {
	if f, ok := f.(interface{ FlushError() error }); ok {
		return f.FlushError()
	}
	if f != nil {
		f.Flush()
	}
	return nil
}

// stop drains pending bytes and joins the timer before the handler can return.
func (fw *flushWriter) stop() error {
	fw.mu.Lock()
	if !fw.stopped {
		fw.stopped = true
		fw.flushLocked()
		if fw.done != nil {
			close(fw.done)
		}
	}
	err, exited := fw.err, fw.exited
	fw.mu.Unlock()
	if exited != nil {
		<-exited
	}
	return err
}

// sseInterceptWriter wraps a writer and scans the SSE stream for token_count events.
// It passes all data through to the underlying writer while extracting token data inline.
//
// The optional onEvent hook can divert the stream: returning drop=true
// causes the offending event's bytes to be withheld from the output;
// returning terminate=true tells the writer to stop forwarding any
// further upstream bytes (the writer's caller is responsible for
// emitting any synthetic terminal events directly to the wrapped
// writer afterwards). When onEvent is set the writer buffers each
// event boundary before flushing, so a drop decision actually
// suppresses the bytes rather than chasing them after they're on the
// wire.
const sseInterceptMaxBufferedBytes = 64 * 1024

type sseInterceptWriter struct {
	w           io.Writer
	buf         []byte
	framer      sse.Framer
	err         error
	callback    func(eventData []byte)
	onEvent     func(eventData []byte) (drop bool, terminate bool)
	terminated  bool
	lastDropped bool
}

func (sw *sseInterceptWriter) Write(p []byte) (int, error) {
	if sw.err != nil {
		return 0, sw.err
	}
	if sw.onEvent == nil {
		// Legacy mode: write-through, then scan after the fact. This
		// matches the long-standing behavior used by every non-Codex
		// caller that just wants usage extraction.
		n, err := sw.w.Write(p)
		// Usage describes upstream work even when the client write fails.
		sw.buf = append(sw.buf, p...)
		sw.scanForEventsLegacy()
		if err == nil && n != len(p) {
			err = io.ErrShortWrite
		}
		if err != nil {
			sw.err = err
		}
		return n, sw.err
	}

	// Suppression mode: buffer the bytes first, scan for full event
	// boundaries, and only forward the bytes the inspector approves.
	if sw.terminated {
		// We already decided to stop forwarding; pretend we wrote
		// everything so the upstream copy loop drains and ends
		// without back-pressure stalling the relay.
		return len(p), nil
	}
	sw.buf = append(sw.buf, p...)
	for {
		event, advance, ok := sw.takeNextEvent()
		if !ok {
			if len(sw.buf) > sseInterceptMaxBufferedBytes {
				sw.err = fmt.Errorf("SSE event exceeded %d bytes without a terminator", sseInterceptMaxBufferedBytes)
				sw.buf = nil
				return len(p), sw.err
			}
			break
		}
		eventBytes := append([]byte(nil), sw.buf[:advance]...)
		sw.buf = sw.buf[advance:]
		if event == nil {
			// Pseudo-event: the pairing LF of a CRLF terminator that
			// straddled two reads. Its bytes belong to the terminator of
			// the PREVIOUS event, so it inherits that event's drop
			// decision — otherwise suppression would leak a stray blank
			// line depending on how the stream happened to be segmented.
			if !sw.lastDropped {
				sse.Write(sw.w, eventBytes, &sw.err)
				if sw.err != nil {
					return len(p), sw.err
				}
			}
			continue
		}
		drop, terminate := sw.invokeInspect(event)
		sw.lastDropped = drop
		if !drop {
			sse.Write(sw.w, eventBytes, &sw.err)
			if sw.err != nil {
				return len(p), sw.err
			}
		}
		if terminate {
			sw.terminated = true
			sw.buf = nil
			return len(p), nil
		}
	}
	return len(p), nil
}

func (sw *sseInterceptWriter) takeNextEvent() (event []byte, advance int, ok bool) {
	return sw.framer.Next(sw.buf)
}

func (sw *sseInterceptWriter) invokeInspect(event []byte) (drop, terminate bool) {
	data := sse.ExtractEventData(event)
	if len(data) == 0 {
		return false, false
	}
	if sw.callback != nil {
		sw.callback(data)
	}
	if sw.onEvent != nil {
		drop, terminate = sw.onEvent(data)
	}
	return drop, terminate
}

func (sw *sseInterceptWriter) scanForEventsLegacy() {
	for {
		event, advance, ok := sw.takeNextEvent()
		if !ok {
			return
		}
		sw.processEvent(event)
		sw.buf = sw.buf[advance:]
	}
}

func (sw *sseInterceptWriter) processEvent(event []byte) {
	if sw.callback == nil {
		return
	}
	data := sse.ExtractEventData(event)
	if len(data) > 0 {
		sw.callback(data)
	}
}
