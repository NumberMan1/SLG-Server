package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"sync/atomic"

	"google.golang.org/protobuf/proto"

	"github.com/NumberMan1/slg-server/infrastructure/servicediscovery"
	sessionpb "github.com/NumberMan1/slg-server/proto"
)

// CheckLoginReady 查询服务发现中的登录实例，确认数据库和票据存储可用。
func CheckLoginReady(ctx context.Context, addresses []string, client *http.Client) error {
	for _, address := range addresses {
		upstream, err := servicediscovery.ParseOrigin(address)
		if err != nil {
			continue
		}
		upstream.Path, upstream.RawPath = "/readyz", ""
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream.String(), nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err != nil {
			continue
		}
		_ = response.Body.Close()
		if response.StatusCode == http.StatusNoContent {
			return nil
		}
	}
	return errors.New("no ready login service")
}

// WithLogin 转发注册和登录请求，并在登录回包中附上网关实际监听的 TCP 端口。
func WithLogin(base http.Handler, addresses func() []string, client *http.Client, listener interface{ Addr() net.Addr }) http.Handler {
	const maxBody = 1024
	const protobufType = "application/protobuf"
	var next atomic.Uint64
	mux := http.NewServeMux()
	mux.Handle("/", base)
	forward := func(w http.ResponseWriter, r *http.Request, path string) (*http.Response, []byte) {
		w.Header().Set("Cache-Control", "no-store")
		mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mediaType != protobufType {
			http.Error(w, "protobuf required", http.StatusUnsupportedMediaType)
			return nil, nil
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			http.Error(w, "invalid account request", http.StatusBadRequest)
			return nil, nil
		}
		servers := addresses()
		if len(servers) == 0 {
			http.Error(w, "account service unavailable", http.StatusServiceUnavailable)
			return nil, nil
		}
		upstream, err := servicediscovery.ParseOrigin(servers[(next.Add(1)-1)%uint64(len(servers))])
		if err != nil {
			http.Error(w, "account service unavailable", http.StatusServiceUnavailable)
			return nil, nil
		}
		// 写操作只选择一次上游，转发出错时由客户端决定是否重试。
		upstream.Path, upstream.RawPath = path, ""
		request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstream.String(), bytes.NewReader(body))
		if err != nil {
			http.Error(w, "account service unavailable", http.StatusServiceUnavailable)
			return nil, nil
		}
		request.Header.Set("Content-Type", protobufType)
		response, err := client.Do(request)
		if err != nil {
			http.Error(w, "account service unavailable", http.StatusServiceUnavailable)
			return nil, nil
		}
		defer response.Body.Close()
		payload, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
		if err != nil || len(payload) > maxBody {
			http.Error(w, "account service unavailable", http.StatusServiceUnavailable)
			return nil, nil
		}
		return response, payload
	}
	mux.HandleFunc("POST /v1/register", func(w http.ResponseWriter, r *http.Request) {
		response, _ := forward(w, r, "/internal/v1/register")
		if response == nil {
			return
		}
		switch response.StatusCode {
		case http.StatusCreated:
			w.WriteHeader(http.StatusCreated)
		case http.StatusBadRequest:
			http.Error(w, "invalid registration credentials", http.StatusBadRequest)
		case http.StatusConflict:
			http.Error(w, "account already exists", http.StatusConflict)
		case http.StatusTooManyRequests:
			w.Header().Set("Retry-After", response.Header.Get("Retry-After"))
			http.Error(w, "registration rate exceeded", http.StatusTooManyRequests)
		default:
			http.Error(w, "registration unavailable", http.StatusServiceUnavailable)
		}
	})
	mux.HandleFunc("POST /v1/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		// 登录回包必须包含可用的 TCP 端口，注册不依赖长连接监听器。
		addr, ok := listener.Addr().(*net.TCPAddr)
		if !ok {
			http.Error(w, "login unavailable", http.StatusServiceUnavailable)
			return
		}
		response, payload := forward(w, r, "/internal/v1/login")
		if response == nil {
			return
		}
		switch response.StatusCode {
		case http.StatusBadRequest:
			http.Error(w, "invalid login request", http.StatusBadRequest)
			return
		case http.StatusUnauthorized:
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		case http.StatusOK:
		default:
			http.Error(w, "login unavailable", http.StatusServiceUnavailable)
			return
		}
		// 只接受登录服签发的有效票据，客户端连接端口由网关填写。
		var ticket sessionpb.LoginReply
		if response.Header.Get("Content-Type") != protobufType ||
			proto.Unmarshal(payload, &ticket) != nil || ticket.GetTicket() == "" {
			http.Error(w, "login unavailable", http.StatusServiceUnavailable)
			return
		}
		reply, err := proto.Marshal(&sessionpb.LoginReply{Ticket: ticket.GetTicket(), TcpPort: uint32(addr.Port)})
		if err != nil {
			http.Error(w, "login failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", protobufType)
		_, _ = w.Write(reply)
	})
	return mux
}
