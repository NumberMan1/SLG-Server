// Package loginserver 创建账号、校验凭据并签发一次性登录票据。
package loginserver

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"sync"
	"time"

	serverlog "github.com/NumberMan1/log"
	"github.com/NumberMan1/log/field"
	"google.golang.org/protobuf/proto"

	"github.com/NumberMan1/slg-server/infrastructure/httpserver"
	"github.com/NumberMan1/slg-server/infrastructure/servicediscovery"
	sessionpb "github.com/NumberMan1/slg-server/proto"
)

type accountStore interface {
	Authenticate(ctx context.Context, account, password string) error
	Register(ctx context.Context, account, password string) error
}

type ticketIssuer interface {
	Issue(ctx context.Context, account string) (string, error)
}

type credentials interface {
	proto.Message
	GetAccount() string
	GetPassword() string
}

const protobufType = "application/protobuf"

// AdmissionLimits 为登录和注册预留独立并发槽位。注册速率为 0 时关闭每秒限额。
type AdmissionLimits struct {
	LoginConcurrency        int
	RegistrationConcurrency int
	RegistrationRate        int
}

// DefaultAdmissionLimits 返回本地联调使用的并发容量和注册速率。
func DefaultAdmissionLimits() AdmissionLimits {
	return AdmissionLimits{LoginConcurrency: 16, RegistrationConcurrency: 4}
}

type registrationLimit struct {
	mu          sync.Mutex
	perSecond   int
	windowStart time.Time
	used        int
}

func (l *registrationLimit) allow(now time.Time) bool {
	if l.perSecond == 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.windowStart) >= time.Second {
		l.windowStart, l.used = now, 0
	}
	if l.used >= l.perSecond {
		return false
	}
	l.used++
	return true
}

// NewHandler 提供内部注册、登录和健康检查接口，并使用 limits 中的正整数配置各自的并发容量。
func NewHandler(accounts accountStore, tickets ticketIssuer, logger serverlog.Logger, limits AdmissionLimits) http.Handler {
	mux := http.NewServeMux()
	loginSlots := make(chan struct{}, limits.LoginConcurrency)
	registrationSlots := make(chan struct{}, limits.RegistrationConcurrency)
	registrationRate := registrationLimit{perSecond: limits.RegistrationRate}
	mux.HandleFunc("GET /healthz", httpserver.Health(servicediscovery.LoginServer))
	mux.HandleFunc("POST /internal/v1/login", credentialHandler(loginSlots,
		func() credentials { return &sessionpb.LoginRequest{} },
		func(w http.ResponseWriter, ctx context.Context, account, password string) {
			err := accounts.Authenticate(ctx, account, password)
			if errors.Is(err, ErrCredentials) {
				http.Error(w, "invalid credentials", http.StatusUnauthorized)
				return
			}
			if err != nil {
				logger.Error("account lookup failed", field.Any("error", err))
				http.Error(w, "login unavailable", http.StatusServiceUnavailable)
				return
			}
			ticket, err := tickets.Issue(ctx, account)
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
		}))
	mux.HandleFunc("POST /internal/v1/register", credentialHandler(registrationSlots,
		func() credentials { return &sessionpb.RegisterRequest{} },
		func(w http.ResponseWriter, ctx context.Context, account, password string) {
			// 启用注册速率时，先完成频率检查再计算 bcrypt。
			if !registrationRate.allow(time.Now()) {
				w.Header().Set("Retry-After", "1")
				http.Error(w, "registration rate exceeded", http.StatusTooManyRequests)
				return
			}
			err := accounts.Register(ctx, account, password)
			switch {
			case err == nil:
				w.WriteHeader(http.StatusCreated)
			case errors.Is(err, ErrCredentials):
				http.Error(w, "invalid registration credentials", http.StatusBadRequest)
			case errors.Is(err, ErrAccountExists):
				http.Error(w, "account already exists", http.StatusConflict)
			default:
				logger.Error("account creation failed", field.Any("error", err))
				http.Error(w, "registration unavailable", http.StatusServiceUnavailable)
			}
		}))
	return mux
}

func credentialHandler(slots chan struct{}, newMessage func() credentials,
	handle func(http.ResponseWriter, context.Context, string, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		message := newMessage()
		if !readCredentials(w, r, message) {
			return
		}
		// 各入口独立限制同时进行的密码计算，满载时立即返回。
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			http.Error(w, "account service capacity reached", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		handle(w, ctx, message.GetAccount(), message.GetPassword())
	}
}

func readCredentials(w http.ResponseWriter, r *http.Request, message proto.Message) bool {
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType != protobufType {
		http.Error(w, "protobuf required", http.StatusUnsupportedMediaType)
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
	if err != nil || proto.Unmarshal(body, message) != nil {
		http.Error(w, "invalid account request", http.StatusBadRequest)
		return false
	}
	return true
}
