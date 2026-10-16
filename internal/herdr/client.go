// Package herdr is a client for herdr's JSON socket API.
//
// The protocol is newline-delimited JSON over a Unix socket: one request
// {id, method, params} per connection, answered by {id, result} or {id, error}.
// events.subscribe keeps its connection open and streams {event, data} lines.
// Method and field names follow `herdr api schema --json` (protocol 22).
package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"syscall"
	"time"
)

// SocketEnv is the variable herdr injects into every pane and plugin process.
const SocketEnv = "HERDR_SOCKET_PATH"

// Client talks to exactly one herdr server. It holds no connection; each call dials.
type Client struct {
	socket  string
	seq     atomic.Uint64
	timeout time.Duration // callTimeout; shorter in tests
}

// callTimeout bounds a request whose context has no deadline, so that a
// server that accepts the connection but never answers cannot hang the
// caller. A request that waits on the server (agent.start, and agent.prompt
// or agent.wait with a wait) gets its own wait on top.
const callTimeout = 30 * time.Second

// New returns a client for the server listening on socket.
func New(socket string) *Client { return &Client{socket: socket, timeout: callTimeout} }

// Socket reports the socket path this client targets.
func (c *Client) Socket() string { return c.socket }

// APIError is an error envelope returned by the server, e.g. agent_not_ready.
type APIError struct {
	Method  string
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("herdr %s: %s: %s", e.Method, e.Code, e.Message)
}

// IsCode reports whether err is an APIError with the given code.
func IsCode(err error, code string) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Code == code
}

// Unreachable reports whether err is a failure to connect to the server:
// it is down, or this process may not reach its socket (Denied).
func Unreachable(err error) bool {
	var oe *net.OpError
	return errors.As(err, &oe) && oe.Op == "dial"
}

// Denied reports whether err is a connection this process was not allowed
// to make, as in a sandbox: Codex runs an agent's commands in one that
// forbids connecting to Unix sockets outside its workspace.
func Denied(err error) bool {
	return Unreachable(err) && (errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES))
}

// Reachable connects to the server's socket and hangs up.
func (c *Client) Reachable(ctx context.Context) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.socket)
	if err != nil {
		return fmt.Errorf("herdr: dial %s: %w", c.socket, err)
	}
	return conn.Close()
}

type request struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

type response struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *APIError       `json:"error"`
}

func (c *Client) dial(ctx context.Context, method string, params any) (net.Conn, *bufio.Reader, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.socket)
	if err != nil {
		return nil, nil, fmt.Errorf("herdr %s: dial %s: %w", method, c.socket, err)
	}
	if params == nil {
		params = struct{}{}
	}
	req := request{ID: fmt.Sprintf("routr-%d", c.seq.Add(1)), Method: method, Params: params}
	b, err := json.Marshal(req)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("herdr %s: encode: %w", method, err)
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("herdr %s: write: %w", method, err)
	}
	return conn, bufio.NewReaderSize(conn, 64*1024), nil
}

// call performs one request and decodes result into out (which may be nil),
// within ctx's deadline or else the client's timeout.
func (c *Client) call(ctx context.Context, method string, params, out any) error {
	return c.callWithin(ctx, c.timeout, method, params, out)
}

// callWithin is call bounded by limit when ctx has no deadline; limit 0
// leaves it unbounded (a server-side wait the caller did not bound).
func (c *Client) callWithin(ctx context.Context, limit time.Duration, method string, params, out any) error {
	if _, ok := ctx.Deadline(); !ok && limit > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limit)
		defer cancel()
	}
	conn, r, err := c.dial(ctx, method, params)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	line, err := r.ReadBytes('\n')
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("herdr %s: %w", method, ctx.Err())
		}
		return fmt.Errorf("herdr %s: read: %w", method, err)
	}
	var resp response
	if err := json.Unmarshal(line, &resp); err != nil {
		return fmt.Errorf("herdr %s: decode response: %w", method, err)
	}
	if resp.Error != nil {
		resp.Error.Method = method
		return resp.Error
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(resp.Result, out); err != nil {
		return fmt.Errorf("herdr %s: decode result: %w", method, err)
	}
	return nil
}
