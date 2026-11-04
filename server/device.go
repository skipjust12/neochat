package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"golang.org/x/net/websocket"

	"neochat/auth"
	"neochat/device"
)

// The user's computer, connected through the NeoChat desktop app
// (cmd/neochat-desktop, package device). With "Execute commands on" set to
// Computer, the model's terminal tools run there instead of on this
// server: each call goes to the app as a device.Request and comes back as
// a device.Result. One computer per user: a new connection replaces the
// old one.

type deviceHub struct {
	mu     sync.Mutex
	byUser map[string]*deviceConn
}

type deviceConn struct {
	userID string
	ws     *websocket.Conn

	writeMu sync.Mutex
	mu      sync.Mutex
	hello   device.Hello
	waiting map[string]chan device.Result
	closed  chan struct{}
	once    sync.Once
}

// errDeviceGone is a call to a computer that disconnected.
var errDeviceGone = errors.New("the computer disconnected (is the NeoChat app still open?)")

func (s *Server) devices() *deviceHub {
	s.extrasMu.Lock()
	defer s.extrasMu.Unlock()
	if s.deviceHub == nil {
		s.deviceHub = &deviceHub{byUser: map[string]*deviceConn{}}
	}
	return s.deviceHub
}

// device is the user's connected computer, nil when there is none.
func (h *deviceHub) device(userID string) *deviceConn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.byUser[userID]
}

func (h *deviceHub) add(d *deviceConn) {
	h.mu.Lock()
	old := h.byUser[d.userID]
	h.byUser[d.userID] = d
	h.mu.Unlock()
	if old != nil {
		old.close()
	}
}

func (h *deviceHub) remove(d *deviceConn) {
	h.mu.Lock()
	if h.byUser[d.userID] == d {
		delete(h.byUser, d.userID)
	}
	h.mu.Unlock()
}

func (d *deviceConn) info() device.Hello {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.hello
}

func (d *deviceConn) send(m device.Message) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	_ = d.ws.SetWriteDeadline(time.Now().Add(60 * time.Second))
	return websocket.JSON.Send(d.ws, m)
}

func (d *deviceConn) close() {
	d.once.Do(func() {
		close(d.closed)
		_ = d.ws.Close()
	})
}

// call runs one request on the computer and waits for its result. A
// cancelled ctx (Stop) cancels it there too.
func (d *deviceConn) call(ctx context.Context, req device.Request) (device.Result, error) {
	req.ID = newDeviceID()
	ch := make(chan device.Result, 1)
	d.mu.Lock()
	d.waiting[req.ID] = ch
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.waiting, req.ID)
		d.mu.Unlock()
	}()
	if err := d.send(device.Message{Type: device.TypeRun, Request: &req}); err != nil {
		return device.Result{}, errDeviceGone
	}
	select {
	case res := <-ch:
		return res, nil
	case <-d.closed:
		return device.Result{}, errDeviceGone
	case <-ctx.Done():
		_ = d.send(device.Message{Type: device.TypeCancel, ID: req.ID})
		return device.Result{}, ctx.Err()
	}
}

func newDeviceID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// handleDeviceConnect is GET /device/connect: the desktop app's WebSocket.
// Only an API key opens it, never the browser's session cookie, so no web
// page can pose as the user's computer.
func (s *Server) handleDeviceConnect(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	if r.Header.Get("Authorization") == "" {
		http.Error(w, "the desktop app connects with an API key", http.StatusUnauthorized)
		return
	}
	hub := s.devices()
	websocket.Server{
		// The app isn't a browser: its Origin proves nothing, the key does.
		Handshake: func(*websocket.Config, *http.Request) error { return nil },
		Handler: func(ws *websocket.Conn) {
			// http.Server's read deadline for the request would end the
			// connection; the pings below keep it honest instead.
			_ = ws.SetDeadline(time.Time{})
			ws.MaxPayloadBytes = device.MaxMessageBytes
			d := &deviceConn{userID: identity.UserID, ws: ws, waiting: map[string]chan device.Result{}, closed: make(chan struct{})}
			defer d.close()

			_ = ws.SetReadDeadline(time.Now().Add(20 * time.Second))
			var first device.Message
			if err := websocket.JSON.Receive(ws, &first); err != nil || first.Type != device.TypeHello || first.Hello == nil {
				log.Printf("server: device for user_id=%s sent no hello: %v", identity.UserID, err)
				return
			}
			d.hello = boundHello(*first.Hello)
			hub.add(d)
			defer hub.remove(d)
			log.Printf("server: computer %q (%s) connected for user_id=%s, %d MCP servers", d.hello.Name, d.hello.OS, identity.UserID, len(d.hello.MCP))

			go func() {
				ticker := time.NewTicker(device.PingEvery)
				defer ticker.Stop()
				for {
					select {
					case <-d.closed:
						return
					case <-ticker.C:
						if d.send(device.Message{Type: device.TypePing}) != nil {
							d.close()
							return
						}
					}
				}
			}()
			for {
				_ = ws.SetReadDeadline(time.Now().Add(device.DeadAfter))
				var m device.Message
				if err := websocket.JSON.Receive(ws, &m); err != nil {
					log.Printf("server: computer %q for user_id=%s disconnected: %v", d.hello.Name, identity.UserID, err)
					return
				}
				switch m.Type {
				case device.TypePing:
					_ = d.send(device.Message{Type: device.TypePong})
				case device.TypeTools:
					d.mu.Lock()
					d.hello.MCP = boundHello(device.Hello{MCP: m.Tools}).MCP
					d.mu.Unlock()
				case device.TypeResult:
					if m.Result == nil {
						continue
					}
					d.mu.Lock()
					ch := d.waiting[m.Result.ID]
					d.mu.Unlock()
					if ch != nil {
						select {
						case ch <- *m.Result:
						default:
						}
					}
				}
			}
		},
	}.ServeHTTP(w, r)
}

// boundHello keeps what a computer says about itself to sane sizes.
func boundHello(h device.Hello) device.Hello {
	h.Name, h.OS, h.Description = clipRunes(h.Name, 100), clipRunes(h.OS, 40), clipRunes(h.Description, 400)
	h.User, h.Home, h.WorkDir = clipRunes(h.User, 100), clipRunes(h.Home, 400), clipRunes(h.WorkDir, 400)
	if len(h.Shells) > 8 {
		h.Shells = h.Shells[:8]
	}
	if len(h.Languages) > 8 {
		h.Languages = h.Languages[:8]
	}
	if len(h.MCP) > 20 {
		h.MCP = h.MCP[:20]
	}
	for i := range h.MCP {
		h.MCP[i].Name, h.MCP[i].Error = clipRunes(h.MCP[i].Name, 60), clipRunes(h.MCP[i].Error, 300)
		if len(h.MCP[i].Tools) > 80 {
			h.MCP[i].Tools = h.MCP[i].Tools[:80]
		}
		for j := range h.MCP[i].Tools {
			t := &h.MCP[i].Tools[j]
			t.Name, t.Description = clipRunes(t.Name, 100), clipRunes(t.Description, 1500)
			if len(t.InputSchema) > 30_000 {
				t.InputSchema = nil
			}
		}
	}
	return h
}

// handleDeviceStatus is GET /device/status: whether the user's computer
// is connected, for the composer's "Execute commands on" switch.
func (s *Server) handleDeviceStatus(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	type mcpServer struct {
		Name  string `json:"name"`
		Tools int    `json:"tools"`
		Error string `json:"error,omitempty"`
	}
	out := struct {
		Connected   bool        `json:"connected"`
		Name        string      `json:"name,omitempty"`
		OS          string      `json:"os,omitempty"`
		Description string      `json:"description,omitempty"`
		MCP         []mcpServer `json:"mcp,omitempty"`
	}{}
	if d := s.devices().device(identity.UserID); d != nil {
		hello := d.info()
		out.Connected, out.Name, out.OS, out.Description = true, hello.Name, hello.OS, hello.Description
		for _, server := range hello.MCP {
			out.MCP = append(out.MCP, mcpServer{Name: server.Name, Tools: len(server.Tools), Error: server.Error})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// --- approvals ---

// approvalHub holds the actions waiting for the user's yes or no
// ("Ask for approval" in the composer's + menu).
type approvalHub struct {
	mu      sync.Mutex
	pending map[string]pendingApproval
}

type pendingApproval struct {
	userID string
	answer chan bool
}

// approvalTimeout is how long an action waits for the user; then it is
// denied, as unanswered.
const approvalTimeout = 5 * time.Minute

func (s *Server) approvals() *approvalHub {
	s.extrasMu.Lock()
	defer s.extrasMu.Unlock()
	if s.approvalHub == nil {
		s.approvalHub = &approvalHub{pending: map[string]pendingApproval{}}
	}
	return s.approvalHub
}

func (h *approvalHub) open(userID string) (string, chan bool) {
	id := newDeviceID()
	ch := make(chan bool, 1)
	h.mu.Lock()
	h.pending[id] = pendingApproval{userID: userID, answer: ch}
	h.mu.Unlock()
	return id, ch
}

func (h *approvalHub) close(id string) {
	h.mu.Lock()
	delete(h.pending, id)
	h.mu.Unlock()
}

func (h *approvalHub) answer(userID, id string, allow bool) bool {
	h.mu.Lock()
	p, ok := h.pending[id]
	h.mu.Unlock()
	if !ok || p.userID != userID {
		return false
	}
	select {
	case p.answer <- allow:
	default:
	}
	return true
}

// handleApprove is POST /chat/approve: {"id": ..., "allow": true|false}.
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var input struct {
		ID    string `json:"id"`
		Allow bool   `json:"allow"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.ID == "" {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if !s.approvals().answer(identity.UserID, input.ID, input.Allow) {
		http.Error(w, "this action is no longer waiting for approval", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
