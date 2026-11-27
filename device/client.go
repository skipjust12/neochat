package device

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

func osName() string { return runtime.GOOS + "/" + runtime.GOARCH }

// Ping timing: each side pings every PingEvery and gives up on a
// connection silent for DeadAfter.
const (
	PingEvery = 25 * time.Second
	DeadAfter = 70 * time.Second
)

// ConnectURL is the device endpoint of a NeoChat server ("https://host"
// becomes "wss://host/device/connect").
func ConnectURL(server string) (string, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(server), "/"))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("not a server address: %q", server)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("the server address must start with https:// or http://: %q", server)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/device/connect"
	return u.String(), nil
}

// Client keeps this computer connected to the server and runs what it
// sends. Status, when set, hears about connecting and disconnecting.
type Client struct {
	Server   string // https://...
	Key      string // the user's NeoChat API key
	Executor *Executor
	Version  string
	Status   func(connected bool, err error)

	toolsOnce sync.Once
	tools     chan struct{}
}

// ToolsChanged tells the server the MCP tools changed (a server finished
// starting), when connected; on the next connection the Hello has them.
func (c *Client) ToolsChanged() {
	select {
	case c.toolsChan() <- struct{}{}:
	default:
	}
}

func (c *Client) toolsChan() chan struct{} {
	c.toolsOnce.Do(func() { c.tools = make(chan struct{}, 1) })
	return c.tools
}

// Run stays connected until ctx ends, reconnecting with backoff.
func (c *Client) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		started := time.Now()
		err := c.session(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if c.Status != nil {
			c.Status(false, err)
		}
		var authErr *AuthError
		if errors.As(err, &authErr) {
			return err
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		log.Printf("device: disconnected (%v); reconnecting in %s", err, backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// AuthError is the server refusing the key: reconnecting won't help.
type AuthError struct{ Status int }

func (e *AuthError) Error() string {
	return fmt.Sprintf("the server refused the API key (HTTP %d)", e.Status)
}

func (c *Client) session(ctx context.Context) error {
	target, err := ConnectURL(c.Server)
	if err != nil {
		return err
	}
	origin := strings.Replace(strings.Replace(target, "wss://", "https://", 1), "ws://", "http://", 1)
	config, err := websocket.NewConfig(target, origin)
	if err != nil {
		return err
	}
	config.Header = http.Header{"Authorization": {"Bearer " + c.Key}}
	dialCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	ws, err := config.DialContext(dialCtx)
	cancel()
	if err != nil {
		var dialErr *websocket.DialError
		if errors.As(err, &dialErr) && errors.Is(dialErr.Err, websocket.ErrBadStatus) {
			if status := probeStatus(ctx, c.Server, c.Key); status == http.StatusUnauthorized || status == http.StatusForbidden {
				return &AuthError{Status: status}
			}
		}
		return err
	}
	defer ws.Close()
	ws.MaxPayloadBytes = MaxMessageBytes

	var writeMu sync.Mutex
	send := func(m Message) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = ws.SetWriteDeadline(time.Now().Add(60 * time.Second))
		return websocket.JSON.Send(ws, m)
	}
	hello := c.Executor.Hello(c.Version)
	if err := send(Message{Type: TypeHello, Hello: &hello}); err != nil {
		return err
	}
	log.Printf("device: connected to %s as %s", c.Server, hello.Name)
	if c.Status != nil {
		c.Status(true, nil)
	}

	sessionCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		ticker := time.NewTicker(PingEvery)
		defer ticker.Stop()
		for {
			select {
			case <-sessionCtx.Done():
				_ = ws.Close()
				return
			case <-ticker.C:
				if send(Message{Type: TypePing}) != nil {
					_ = ws.Close()
					return
				}
			case <-c.toolsChan():
				_ = send(Message{Type: TypeTools, Tools: c.Executor.MCPServers()})
			}
		}
	}()

	var runningMu sync.Mutex
	running := map[string]context.CancelFunc{}
	defer func() {
		runningMu.Lock()
		for _, cancel := range running {
			cancel()
		}
		runningMu.Unlock()
	}()
	for {
		_ = ws.SetReadDeadline(time.Now().Add(DeadAfter))
		var m Message
		if err := websocket.JSON.Receive(ws, &m); err != nil {
			return err
		}
		switch m.Type {
		case TypePing:
			_ = send(Message{Type: TypePong})
		case TypeCancel:
			runningMu.Lock()
			if cancel := running[m.ID]; cancel != nil {
				cancel()
			}
			runningMu.Unlock()
		case TypeRun:
			if m.Request == nil || m.Request.ID == "" {
				continue
			}
			req := *m.Request
			reqCtx, cancel := context.WithCancel(sessionCtx)
			runningMu.Lock()
			running[req.ID] = cancel
			runningMu.Unlock()
			go func() {
				defer cancel()
				log.Printf("device: %s %s", req.Op, describeRequest(req))
				res := c.Executor.Run(reqCtx, req)
				runningMu.Lock()
				delete(running, req.ID)
				runningMu.Unlock()
				if err := send(Message{Type: TypeResult, Result: &res}); err != nil {
					log.Printf("device: send result: %v", err)
				}
			}()
		}
	}
}

// probeStatus asks the server, over plain HTTP, how it takes the key, to
// tell a refused key from a server that is down.
func probeStatus(ctx context.Context, server, key string) int {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(server, "/")+"/device/status", nil)
	if err != nil {
		return 0
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

func describeRequest(r Request) string {
	text := r.Command + r.Path + r.Tool
	if r.Op == OpRunScript {
		text = r.Language + " script"
	}
	if r.Op == OpMCPCall {
		text = r.Server + "/" + r.Tool
	}
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 120 {
		text = text[:120] + "…"
	}
	return text
}
