package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/NumberMan1/orbit/rpc"
	"github.com/NumberMan1/orbit/socket"
	"google.golang.org/protobuf/proto"

	sessionpb "github.com/NumberMan1/slg-server/proto"
)

const (
	protocolVersion      = 1
	sessionKey           = "slg.account"
	maxSessionPayload    = 1024
	maxWorldReplyPayload = 8192
)

type ticketConsumer interface {
	Consume(ctx context.Context, ticket string) (string, error)
}

// Sessions 为 TCP 和 WebSocket 连接提供相同的一次性票据认证流程。
type Sessions struct {
	tickets ticketConsumer
}

// NewSessions 使用给定的票据存储创建会话认证处理器。
func NewSessions(tickets ticketConsumer) *Sessions {
	return &Sessions{tickets: tickets}
}

// SessionOptions 配置连接准入、帧大小、发送队列和读写时限。
func SessionOptions() (socket.Options, error) {
	admission, err := socket.NewAdmission(128, 16)
	return socket.Options{
		Admission: admission, MaxPayload: maxWorldReplyPayload, SendQueue: 16,
		ReadTimeout: 15 * time.Second, WriteTimeout: 5 * time.Second,
	}, err
}

// Connect 校验握手帧，消费票据后为连接绑定账号，并按请求序号返回会话信息。
func (s *Sessions) Connect(ctx context.Context, session *socket.Session, frame rpc.Frame) error {
	if _, authenticated := session.Value(sessionKey); authenticated ||
		frame.ID != uint16(sessionpb.SessionMessageId_SESSION_MESSAGE_ID_CONNECT_REQUEST) || frame.Sequence == 0 ||
		len(frame.Payload) > maxSessionPayload {
		return errors.New("invalid connect frame")
	}
	var request sessionpb.ConnectRequest
	if err := proto.Unmarshal(frame.Payload, &request); err != nil || request.GetProtocolVersion() != protocolVersion {
		return errors.New("invalid connect request")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	// 先验证消息类型和协议版本，再消费不可重复使用的票据。
	account, err := s.tickets.Consume(ctx, request.GetTicket())
	if err != nil {
		return fmt.Errorf("consume ticket: %w", err)
	}
	if err := session.SetValue(sessionKey, account); err != nil {
		return err
	}
	body, err := proto.Marshal(&sessionpb.ConnectReply{Account: account, SessionId: session.ID()})
	if err != nil {
		return err
	}
	return session.Send(rpc.Frame{
		ID: uint16(sessionpb.SessionMessageId_SESSION_MESSAGE_ID_CONNECT_REPLY), Sequence: frame.Sequence, Payload: body,
	})
}
