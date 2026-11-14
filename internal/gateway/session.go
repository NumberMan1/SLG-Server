package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/NumberMan1/orbit/rpc"
	"github.com/NumberMan1/orbit/socket"
	"google.golang.org/protobuf/proto"

	sessionpb "github.com/NumberMan1/slg-server/proto"
)

const (
	protocolVersion = 1
	sessionKey      = "slg.account"
	maxLoginBody    = 1024
	maxTickets      = 1024
	ticketLifetime  = 30 * time.Second
)

type Sessions struct {
	account  string
	password [sha256.Size]byte
	enabled  bool
	mu       sync.Mutex
	tickets  map[string]time.Time
}

func NewSessions(account, password string) *Sessions {
	return &Sessions{
		account: account, password: sha256.Sum256([]byte(password)),
		enabled: password != "", tickets: make(map[string]time.Time),
	}
}

func SessionOptions() (socket.Options, error) {
	admission, err := socket.NewAdmission(128, 16)
	return socket.Options{
		Admission: admission, MaxPayload: maxLoginBody, SendQueue: 16,
		ReadTimeout: 15 * time.Second, WriteTimeout: 5 * time.Second,
	}, err
}

func WithLogin(base http.Handler, sessions *Sessions, listener interface{ Addr() net.Addr }) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", base)
	mux.HandleFunc("POST /v1/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		addr, ok := listener.Addr().(*net.TCPAddr)
		if !sessions.enabled || !ok {
			http.Error(w, "login unavailable", http.StatusServiceUnavailable)
			return
		}
		mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mediaType != "application/protobuf" {
			http.Error(w, "protobuf required", http.StatusUnsupportedMediaType)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLoginBody))
		var login sessionpb.LoginRequest
		if err != nil || proto.Unmarshal(body, &login) != nil {
			http.Error(w, "invalid login request", http.StatusBadRequest)
			return
		}
		password := sha256.Sum256([]byte(login.GetPassword()))
		if subtle.ConstantTimeCompare(password[:], sessions.password[:]) != 1 || login.GetAccount() != sessions.account {
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		ticket, ok := sessions.issue()
		if !ok {
			http.Error(w, "login capacity reached", http.StatusServiceUnavailable)
			return
		}
		response, err := proto.Marshal(&sessionpb.LoginReply{Ticket: ticket, TcpPort: uint32(addr.Port)})
		if err != nil {
			http.Error(w, "login failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/protobuf")
		_, _ = w.Write(response)
	})
	return mux
}

func (s *Sessions) issue() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for ticket, expires := range s.tickets {
		if !now.Before(expires) {
			delete(s.tickets, ticket)
		}
	}
	if len(s.tickets) >= maxTickets {
		return "", false
	}
	ticket := rand.Text()
	s.tickets[ticket] = now.Add(ticketLifetime)
	return ticket, true
}

func (s *Sessions) Connect(_ context.Context, session *socket.Session, frame rpc.Frame) error {
	if _, authenticated := session.Value(sessionKey); authenticated ||
		frame.ID != uint16(sessionpb.SessionMessageId_SESSION_MESSAGE_ID_CONNECT_REQUEST) || frame.Sequence == 0 {
		return errors.New("invalid connect frame")
	}
	var request sessionpb.ConnectRequest
	if err := proto.Unmarshal(frame.Payload, &request); err != nil || request.GetProtocolVersion() != protocolVersion {
		return errors.New("invalid connect request")
	}
	s.mu.Lock()
	expires, exists := s.tickets[request.GetTicket()]
	delete(s.tickets, request.GetTicket())
	valid := exists && time.Now().Before(expires)
	s.mu.Unlock()
	if !valid {
		return errors.New("invalid or expired ticket")
	}
	if err := session.SetValue(sessionKey, s.account); err != nil {
		return err
	}
	body, err := proto.Marshal(&sessionpb.ConnectReply{Account: s.account, SessionId: session.ID()})
	if err != nil {
		return err
	}
	return session.Send(rpc.Frame{
		ID: uint16(sessionpb.SessionMessageId_SESSION_MESSAGE_ID_CONNECT_REPLY), Sequence: frame.Sequence, Payload: body,
	})
}
