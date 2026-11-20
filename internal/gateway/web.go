package gateway

import (
	"context"
	"net/http"

	"github.com/NumberMan1/orbit/socket"
	orbitws "github.com/NumberMan1/orbit/socket/websocket"
)

// WebSessions mounts Orbit WebSocket sessions on the gateway's HTTP listener.
// Its transport owns upgraded connections, while the gateway owns the listener.
type WebSessions struct {
	transport *orbitws.Server
}

func NewWebSessions(options socket.Options, sessions *Sessions) (*WebSessions, error) {
	transport, err := orbitws.NewServer("127.0.0.1:0", options, sessions.Connect, nil, nil)
	if err != nil {
		return nil, err
	}
	return &WebSessions{transport: transport}, nil
}

func (s *WebSessions) Start(context.Context) error    { return nil }
func (s *WebSessions) Stop(ctx context.Context) error { return s.transport.Stop(ctx) }
func (s *WebSessions) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.transport.ServeHTTP(w, r)
}

func WithWeb(base http.Handler, sessions *WebSessions, root string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /v1/session", sessions)
	mux.Handle("/v1/", base)
	mux.Handle("/healthz", base)
	if root != "" {
		mux.Handle("/", http.FileServer(http.Dir(root)))
	} else {
		mux.Handle("/", base)
	}
	return mux
}
