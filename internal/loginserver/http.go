package loginserver

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	serverlog "github.com/NumberMan1/log"
	"github.com/NumberMan1/log/field"
	"google.golang.org/protobuf/proto"

	"github.com/NumberMan1/slg-server/infrastructure/httpserver"
	"github.com/NumberMan1/slg-server/infrastructure/servicediscovery"
	sessionpb "github.com/NumberMan1/slg-server/proto"
)

type authenticator interface {
	Authenticate(ctx context.Context, account, password string) error
}

type ticketIssuer interface {
	Issue(ctx context.Context, account string) (string, error)
}

func NewHandler(accounts authenticator, tickets ticketIssuer, logger serverlog.Logger) http.Handler {
	mux := http.NewServeMux()
	loginSlots := make(chan struct{}, 16)
	mux.HandleFunc("GET /healthz", httpserver.Health(servicediscovery.LoginServer))
	mux.HandleFunc("POST /internal/v1/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		const protobufType = "application/protobuf"
		mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mediaType != protobufType {
			http.Error(w, "protobuf required", http.StatusUnsupportedMediaType)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
		var login sessionpb.LoginRequest
		if err != nil || proto.Unmarshal(body, &login) != nil {
			http.Error(w, "invalid login request", http.StatusBadRequest)
			return
		}
		select {
		case loginSlots <- struct{}{}:
			defer func() { <-loginSlots }()
		default:
			http.Error(w, "login capacity reached", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		err = accounts.Authenticate(ctx, login.GetAccount(), login.GetPassword())
		if errors.Is(err, ErrCredentials) {
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		if err != nil {
			logger.Error("account lookup failed", field.Any("error", err))
			http.Error(w, "login unavailable", http.StatusServiceUnavailable)
			return
		}
		ticket, err := tickets.Issue(ctx, login.GetAccount())
		if err != nil {
			logger.Error("ticket issuance failed", field.Any("error", err))
			http.Error(w, "login unavailable", http.StatusServiceUnavailable)
			return
		}
		response, err := proto.Marshal(&sessionpb.LoginReply{Ticket: ticket})
		if err != nil {
			http.Error(w, "login failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", protobufType)
		_, _ = w.Write(response)
	})
	return mux
}
