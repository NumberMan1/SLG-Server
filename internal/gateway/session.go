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
	protocolVersion   = 1
	sessionKey        = "slg.account"
	maxSessionPayload = 1024
)

type ticketConsumer interface {
	Consume(ctx context.Context, ticket string) (string, error)
}

type Sessions struct {
	tickets ticketConsumer
}

func NewSessions(tickets ticketConsumer) *Sessions {
	return &Sessions{tickets: tickets}
}

func SessionOptions() (socket.Options, error) {
	admission, err := socket.NewAdmission(128, 16)
	return socket.Options{
		Admission: admission, MaxPayload: maxSessionPayload, SendQueue: 16,
		ReadTimeout: 15 * time.Second, WriteTimeout: 5 * time.Second,
	}, err
}

func (s *Sessions) Connect(ctx context.Context, session *socket.Session, frame rpc.Frame) error {
	if _, authenticated := session.Value(sessionKey); authenticated ||
		frame.ID != uint16(sessionpb.SessionMessageId_SESSION_MESSAGE_ID_CONNECT_REQUEST) || frame.Sequence == 0 {
		return errors.New("invalid connect frame")
	}
	var request sessionpb.ConnectRequest
	if err := proto.Unmarshal(frame.Payload, &request); err != nil || request.GetProtocolVersion() != protocolVersion {
		return errors.New("invalid connect request")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
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
