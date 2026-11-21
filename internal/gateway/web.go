package gateway

import (
	"context"
	"net/http"

	"github.com/NumberMan1/orbit/socket"
	orbitws "github.com/NumberMan1/orbit/socket/websocket"
)

// WebSessions 在网关的 HTTP 监听器上接收 WebSocket 请求，管理升级后的连接。
type WebSessions struct {
	transport *orbitws.Server
}

// NewWebSessions 创建 WebSocket 会话处理器，复用 TCP 会话的帧处理规则。
func NewWebSessions(options socket.Options, handler socket.Handler) (*WebSessions, error) {
	transport, err := orbitws.NewServer("127.0.0.1:0", options, handler, nil, nil)
	if err != nil {
		return nil, err
	}
	return &WebSessions{transport: transport}, nil
}

// Start 满足组件生命周期接口，实际监听由网关的 HTTP 服务负责。
func (s *WebSessions) Start(context.Context) error { return nil }

// Stop 关闭已升级的 WebSocket 连接。
func (s *WebSessions) Stop(ctx context.Context) error { return s.transport.Stop(ctx) }

// ServeHTTP 将收到的升级请求交给 WebSocket 传输层。
func (s *WebSessions) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.transport.ServeHTTP(w, r)
}

// WithWeb 挂载浏览器会话入口，配置静态目录时同时从该地址提供 Web 构建文件。
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
