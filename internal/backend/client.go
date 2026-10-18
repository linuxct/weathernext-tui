// Package backend runs the Python data worker and multiplexes its NDJSON
// messages without placing forecast payloads on terminal stdout.
package backend

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"weathernext/internal/protocol"
)

type EventHandler = func(protocol.Message)

type pendingCall struct {
	result chan protocol.Message
	event  EventHandler
}

// Client owns one long-lived Python process. Calls are correlated by ID and
// the reader accepts arbitrarily large JSON lines (including full forecasts).
type Client struct {
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	w           *bufio.Writer
	wmu         sync.Mutex
	mu          sync.Mutex
	pending     map[string]*pendingCall
	closed      chan struct{}
	processDone chan struct{}
	closeOnce   sync.Once
	readErr     error
	seq         atomic.Uint64
}

func Start(ctx context.Context, root string) (*Client, error) {
	if root == "" {
		var err error
		root, err = FindProjectRoot()
		if err != nil {
			return nil, err
		}
	}
	python, args, err := pythonCommand(root)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, python, append(args, "-m", "weathercore.worker")...)
	cmd.Dir = root
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("start WeatherNext worker stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("start WeatherNext worker stdout: %w", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start Python worker using %q: %w", python, err)
	}
	c := &Client{cmd: cmd, stdin: stdin, w: bufio.NewWriter(stdin), pending: map[string]*pendingCall{}, closed: make(chan struct{}), processDone: make(chan struct{})}
	go c.readLoop(stdout)
	go func() {
		// StdoutPipe must be drained before Wait: Wait closes the pipe. The
		// reader closes c.closed only after reaching EOF or a read failure.
		<-c.closed
		err := cmd.Wait()
		close(c.processDone)
		if err != nil {
			c.fail(fmt.Errorf("Python worker stopped: %w %s", err, strings.TrimSpace(stderr.String())))
		}
	}()
	return c, nil
}

func FindProjectRoot() (string, error) {
	if p := os.Getenv("WEATHERNEXT_HOME"); p != "" {
		if validRoot(p) {
			return filepath.Abs(p)
		}
		return "", fmt.Errorf("WEATHERNEXT_HOME=%q does not contain weathercore/worker.py", p)
	}
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Dir(exe))
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, cwd)
	}
	for _, start := range candidates {
		for p := start; ; p = filepath.Dir(p) {
			if validRoot(p) {
				return filepath.Abs(p)
			}
			parent := filepath.Dir(p)
			if parent == p {
				break
			}
		}
	}
	return "", errors.New("WeatherNext project files were not found; run the app from the project folder or set WEATHERNEXT_HOME to its path")
}

func validRoot(root string) bool {
	if _, err := os.Stat(filepath.Join(root, "weathercore", "worker.py")); err == nil {
		return true
	}
	// Transitional compatibility while the Python worker package is being added.
	if _, err := os.Stat(filepath.Join(root, "forecast.py")); err == nil {
		return true
	}
	return false
}

func pythonCommand(root string) (string, []string, error) {
	var paths []string
	if runtime.GOOS == "windows" {
		paths = append(paths, filepath.Join(root, ".venv", "Scripts", "python.exe"))
	} else {
		paths = append(paths, filepath.Join(root, ".venv", "bin", "python"))
	}
	for _, path := range paths {
		if st, err := os.Stat(path); err == nil && !st.IsDir() {
			return path, nil, nil
		}
	}
	if runtime.GOOS == "windows" {
		if path, err := exec.LookPath("python"); err == nil {
			return path, nil, nil
		}
		if path, err := exec.LookPath("py"); err == nil {
			return path, []string{"-3"}, nil
		}
		return "", nil, errors.New("Python 3 was not found; install project dependencies in .venv or add Python 3 to PATH")
	}
	for _, name := range []string{"python3", "python"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil, nil
		}
	}
	return "", nil, errors.New("Python 3 was not found; install project dependencies in .venv or add Python 3 to PATH")
}

func (c *Client) Call(ctx context.Context, method string, params map[string]any, onEvent EventHandler) (json.RawMessage, error) {
	if c == nil {
		return nil, errors.New("Python worker client is unavailable")
	}
	id, err := randomID(c.seq.Add(1))
	if err != nil {
		return nil, err
	}
	p := &pendingCall{result: make(chan protocol.Message, 1), event: onEvent}
	c.mu.Lock()
	if c.readErr != nil {
		err := c.readErr
		c.mu.Unlock()
		return nil, err
	}
	c.pending[id] = p
	c.mu.Unlock()
	if err := c.write(protocol.NewRequest(id, method, params)); err != nil {
		c.remove(id)
		return nil, err
	}
	select {
	case msg := <-p.result:
		if msg.Type == "error" {
			return nil, decodeWorkerError(msg.Error)
		}
		if msg.Type != "result" {
			return nil, fmt.Errorf("worker returned unexpected message type %q", msg.Type)
		}
		return msg.Data, nil
	case <-ctx.Done():
		c.remove(id)
		_ = c.sendCancel(id)
		return nil, ctx.Err()
	case <-c.closed:
		c.mu.Lock()
		err := c.readErr
		c.mu.Unlock()
		if err == nil {
			err = errors.New("Python worker stopped")
		}
		return nil, err
	}
}

func (c *Client) sendCancel(requestID string) error {
	id, err := randomID(c.seq.Add(1))
	if err != nil {
		return err
	}
	return c.write(protocol.NewRequest(id, "request.cancel", map[string]any{"id": requestID}))
}

func (c *Client) write(req protocol.Request) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err = c.w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write request to Python worker: %w", err)
	}
	if err = c.w.Flush(); err != nil {
		return fmt.Errorf("flush request to Python worker: %w", err)
	}
	return nil
}

func (c *Client) readLoop(r io.Reader) {
	reader := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := reader.ReadBytes('\n')
		if len(bytesTrim(line)) > 0 {
			var msg protocol.Message
			if decErr := json.Unmarshal(bytesTrim(line), &msg); decErr != nil {
				c.fail(fmt.Errorf("invalid JSON from Python worker: %w", decErr))
				return
			}
			c.dispatch(msg)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				c.fail(errors.New("Python worker closed its output"))
			} else {
				c.fail(fmt.Errorf("read Python worker output: %w", err))
			}
			return
		}
	}
}

func bytesTrim(b []byte) []byte { return []byte(strings.TrimSpace(string(b))) }

func (c *Client) dispatch(msg protocol.Message) {
	c.mu.Lock()
	p := c.pending[msg.ID]
	if p != nil && msg.Type != "event" {
		delete(c.pending, msg.ID)
	}
	c.mu.Unlock()
	if p == nil {
		return
	}
	if msg.Type == "event" {
		if p.event != nil {
			p.event(msg)
		}
		return
	}
	p.result <- msg
}

func (c *Client) remove(id string) { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }

func (c *Client) fail(err error) {
	c.mu.Lock()
	if c.readErr != nil {
		c.mu.Unlock()
		return
	}
	c.readErr = err
	for id, p := range c.pending {
		delete(c.pending, id)
		select {
		case p.result <- protocol.Message{Type: "error", Error: json.RawMessage(fmt.Sprintf(`{"code":"worker_stopped","message":%q}`, err.Error()))}:
		default:
		}
	}
	c.mu.Unlock()
	c.closeOnce.Do(func() { close(c.closed) })
}

func decodeWorkerError(raw json.RawMessage) error {
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return fmt.Errorf("Python worker error: %s", string(raw))
	}
	if e.Code == "" {
		e.Code = "worker_error"
	}
	return fmt.Errorf("%s: %s", e.Code, e.Message)
}

func randomID(n uint64) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("create request ID: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	_ = c.stdin.Close()
	<-c.processDone
	return nil
}
