package browserlogin

// A small DevTools-protocol client over Chromium's --remote-debugging-pipe,
// with event dispatch and flat session support, used for the interactive
// login (Session) and for the headless browser download stage.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// Event is a CDP event with its originating session.
type Event struct {
	SessionID string
	Method    string
	Params    json.RawMessage
}

// CDP is a connection to one browser process.
type CDP struct {
	cmd    *exec.Cmd
	w      io.WriteCloser
	r      *bufio.Reader
	nextID int64
	wmu    sync.Mutex

	pmu     sync.Mutex
	pending map[int64]chan rawReply
	events  chan Event
	queue   chan Event // raw events from readLoop; pump moves them to an unbounded buffer
	closed  chan struct{}
	once    sync.Once
	exitErr error
}

// pump forwards events from the reader to consumers through an unbounded
// in-memory buffer, so a burst of intercepted responses never drops a
// Fetch.requestPaused event (a dropped pause would hang that request).
func (c *CDP) pump() {
	var buf []Event
	for {
		var out chan Event
		var next Event
		if len(buf) > 0 {
			out, next = c.events, buf[0]
		}
		select {
		case ev, ok := <-c.queue:
			if !ok {
				for _, e := range buf {
					c.events <- e
				}
				close(c.events)
				return
			}
			buf = append(buf, ev)
		case out <- next:
			buf = buf[1:]
		}
	}
}

type rawReply struct {
	result json.RawMessage
	err    error
}

type wire struct {
	ID        int64           `json:"id,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    any             `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Launch starts a Chromium-based browser with a DevTools pipe. extraArgs are
// appended after the standard flags.
func Launch(ctx context.Context, binary, profileDir string, extraArgs ...string) (*CDP, error) {
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		return nil, err
	}
	cmdIn, ourOut, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	ourIn, cmdOut, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	args := append([]string{
		"--remote-debugging-pipe",
		"--user-data-dir=" + profileDir,
		"--no-first-run", "--no-default-browser-check", "--disable-sync",
		"--password-store=basic",
	}, extraArgs...)
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.ExtraFiles = []*os.File{cmdIn, cmdOut}
	cmd.Env = os.Environ()
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start browser: %w", err)
	}
	cmdIn.Close()
	cmdOut.Close()
	c := &CDP{cmd: cmd, w: ourOut, r: bufio.NewReaderSize(ourIn, 4<<20), pending: map[int64]chan rawReply{}, events: make(chan Event, 64), closed: make(chan struct{}), queue: make(chan Event, 1024)}
	go c.readLoop()
	go c.pump()
	go func() {
		err := cmd.Wait()
		c.pmu.Lock()
		c.exitErr = err
		for id, ch := range c.pending {
			ch <- rawReply{err: errors.New("browser exited")}
			delete(c.pending, id)
		}
		c.pmu.Unlock()
		c.once.Do(func() { close(c.closed) })
	}()
	return c, nil
}

func (c *CDP) readLoop() {
	defer close(c.queue)
	for {
		raw, err := c.r.ReadBytes(0)
		if err != nil {
			return
		}
		raw = raw[:len(raw)-1]
		var m wire
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		if m.ID != 0 {
			c.pmu.Lock()
			ch, ok := c.pending[m.ID]
			delete(c.pending, m.ID)
			c.pmu.Unlock()
			if ok {
				if m.Error != nil {
					ch <- rawReply{err: errors.New(m.Error.Message)}
				} else {
					ch <- rawReply{result: m.Result}
				}
			}
			continue
		}
		if m.Method != "" {
			var params json.RawMessage
			if b, err := json.Marshal(m.Params); err == nil {
				params = b
			}
			c.queue <- Event{SessionID: m.SessionID, Method: m.Method, Params: params}
		}
	}
}

// Events returns the event stream.
func (c *CDP) Events() <-chan Event { return c.events }

// Closed is closed when the browser process exits.
func (c *CDP) Closed() <-chan struct{} { return c.closed }

// Call sends a command (optionally within a session) and waits for the reply.
func (c *CDP) Call(ctx context.Context, sessionID, method string, params any, out any) error {
	id := atomic.AddInt64(&c.nextID, 1)
	ch := make(chan rawReply, 1)
	c.pmu.Lock()
	c.pending[id] = ch
	c.pmu.Unlock()
	b, _ := json.Marshal(wire{ID: id, SessionID: sessionID, Method: method, Params: params})
	c.wmu.Lock()
	_, err := c.w.Write(append(b, 0))
	c.wmu.Unlock()
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return errors.New("browser exited")
	case r := <-ch:
		if r.err != nil {
			return r.err
		}
		if out != nil && len(r.result) > 0 {
			return json.Unmarshal(r.result, out)
		}
		return nil
	}
}

// Send writes a command without waiting for its reply (fire-and-forget),
// used for high-volume acknowledgements such as Fetch.continueRequest.
func (c *CDP) Send(sessionID, method string, params any) error {
	id := atomic.AddInt64(&c.nextID, 1)
	b, _ := json.Marshal(wire{ID: id, SessionID: sessionID, Method: method, Params: params})
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := c.w.Write(append(b, 0))
	return err
}

// Close asks the browser to quit, then kills it if it lingers.
func (c *CDP) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = c.Call(ctx, "", "Browser.close", map[string]any{}, nil)
	c.w.Close()
	select {
	case <-c.closed:
	case <-time.After(3 * time.Second):
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
	}
}
