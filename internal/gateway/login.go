package gateway

import (
	"bytes"
	"io"
	"mime"
	"net"
	"net/http"
	"sync/atomic"

	"google.golang.org/protobuf/proto"

	"github.com/NumberMan1/slg-server/infrastructure/servicediscovery"
	sessionpb "github.com/NumberMan1/slg-server/proto"
)

// WithLogin 转发登录请求，并在回包中附上网关实际监听的 TCP 端口。
func WithLogin(base http.Handler, addresses func() []string, client *http.Client, listener interface{ Addr() net.Addr }) http.Handler {
	const maxBody = 1024
	const protobufType = "application/protobuf"
	var next atomic.Uint64
	mux := http.NewServeMux()
	mux.Handle("/", base)
	mux.HandleFunc("POST /v1/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		// 在接收登录数据前确认网关已有可公布的 TCP 监听端口。
		addr, ok := listener.Addr().(*net.TCPAddr)
		if !ok {
			http.Error(w, "login unavailable", http.StatusServiceUnavailable)
			return
		}
		mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mediaType != protobufType {
			http.Error(w, "protobuf required", http.StatusUnsupportedMediaType)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			http.Error(w, "invalid login request", http.StatusBadRequest)
			return
		}
		servers := addresses()
		if len(servers) == 0 {
			http.Error(w, "login unavailable", http.StatusServiceUnavailable)
			return
		}
		upstream, err := servicediscovery.ParseOrigin(servers[(next.Add(1)-1)%uint64(len(servers))])
		if err != nil {
			http.Error(w, "login unavailable", http.StatusServiceUnavailable)
			return
		}
		upstream.Path, upstream.RawPath = "/internal/v1/login", ""
		request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstream.String(), bytes.NewReader(body))
		if err != nil {
			http.Error(w, "login unavailable", http.StatusServiceUnavailable)
			return
		}
		request.Header.Set("Content-Type", protobufType)
		response, err := client.Do(request)
		if err != nil {
			http.Error(w, "login unavailable", http.StatusServiceUnavailable)
			return
		}
		defer response.Body.Close()
		payload, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
		if err != nil || len(payload) > maxBody {
			http.Error(w, "login unavailable", http.StatusServiceUnavailable)
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
