package gateway

import (
	"context"
	"errors"
	"net"
	"strconv"
	"time"

	"github.com/NumberMan1/orbit/request"
	"github.com/NumberMan1/orbit/rpc"
	"github.com/NumberMan1/orbit/socket"
	"github.com/NumberMan1/orbit/socket/tcp"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "github.com/NumberMan1/slg-server/proto"
)

const (
	worldClientKey  = "slg.world-client"
	maxWorldPayload = 4096
	// 与 C++ 游戏服的 max_pending_per_connection 一致，在网关限制待处理请求。
	maxWorldPending = 8
	worldRequestID  = uint16(pb.WorldMessageId_WORLD_MESSAGE_ID_REQUEST)
	worldReplyID    = uint16(pb.WorldMessageId_WORLD_MESSAGE_ID_REPLY)
	// 仅用于探活，与 C++ 游戏服的内网探活帧一致。
	worldProbeID      = uint16(5)
	worldProbeReplyID = uint16(6)
)

// NewWorldConnector 管理到权威游戏服的 TCP 连接。每次重连创建新的请求匹配器。
func NewWorldConnector(address string) (*socket.Connector, error) {
	host, port, err := net.SplitHostPort(address)
	number, portErr := strconv.ParseUint(port, 10, 16)
	if err != nil || portErr != nil || host == "" || number == 0 {
		return nil, errors.New("SLG_GAME_RPC_UPSTREAM must use host:port")
	}
	options := socket.Options{
		MaxPayload: maxWorldPayload, WriteTimeout: 3 * time.Second,
		OnClose: func(_ context.Context, s *socket.Session, _ error) {
			if client, ok := worldClient(s); ok {
				client.Close()
			}
		},
	}
	dial := func(ctx context.Context) (*socket.Session, error) {
		session, err := tcp.Dial(ctx, address, options, nil)
		if err != nil {
			return nil, err
		}
		client, err := request.New(session, request.Options{
			ResponseIDs: []uint16{worldReplyID, worldProbeReplyID}, MaxPending: maxWorldPending, Timeout: 3 * time.Second,
		})
		if err != nil {
			session.Close()
			return nil, err
		}
		// 请求匹配器在 Session 交给连接器之前就绪，防止首个请求遇到尚未初始化的连接。
		if err := session.SetValue(worldClientKey, client); err != nil {
			client.Close()
			session.Close()
			return nil, err
		}
		return session, nil
	}
	handle := func(ctx context.Context, s *socket.Session, frame rpc.Frame) error {
		client, ok := worldClient(s)
		if !ok {
			return socket.ErrDisconnected
		}
		return client.Handler(nil)(ctx, s, frame)
	}
	return socket.NewConnector(dial, handle, socket.ConnectorOptions{})
}

func worldClient(session *socket.Session) (*request.Client, bool) {
	value, _ := session.Value(worldClientKey)
	client, ok := value.(*request.Client)
	return client, ok
}

// WorldHandler 在已认证的长连接上转发玩法请求，账号由网关从会话中填写。
func (s *Sessions) WorldHandler(upstream *socket.Connector) socket.Handler {
	return func(ctx context.Context, session *socket.Session, frame rpc.Frame) error {
		if frame.ID == uint16(pb.SessionMessageId_SESSION_MESSAGE_ID_CONNECT_REQUEST) {
			return s.Connect(ctx, session, frame)
		}
		value, authenticated := session.Value(sessionKey)
		account, _ := value.(string)
		if !authenticated || account == "" || frame.Sequence == 0 ||
			frame.ID != worldRequestID {
			return errors.New("invalid world frame")
		}
		var request pb.WorldRequest
		if err := proto.Unmarshal(frame.Payload, &request); err != nil {
			return err
		}
		payload, err := requestWorld(ctx, upstream, &pb.WorldCommand{Account: account, Request: &request})
		if err != nil {
			payload, err = proto.Marshal(&pb.WorldReply{Result: pb.WorldResult_WORLD_RESULT_UNAVAILABLE})
			if err != nil {
				return err
			}
		}
		return session.Send(rpc.Frame{
			ID: worldReplyID, Sequence: frame.Sequence, Payload: payload,
		})
	}
}

func connectedWorldClient(upstream *socket.Connector) (*request.Client, error) {
	session, ok := upstream.Session()
	if !ok {
		return nil, socket.ErrDisconnected
	}
	client, ok := worldClient(session)
	if !ok {
		return nil, socket.ErrDisconnected
	}
	return client, nil
}

// CheckWorldReady 通过实际玩法连接往返探活帧，验证 Orbit 两端能够交换消息。
func CheckWorldReady(ctx context.Context, upstream *socket.Connector) error {
	client, err := connectedWorldClient(upstream)
	if err != nil {
		return err
	}
	reply, err := client.Call(ctx, rpc.Frame{ID: worldProbeID}, worldProbeReplyID)
	if err == nil && len(reply.Payload) != 0 {
		return errors.New("battle server unavailable")
	}
	return err
}

func requestWorld(ctx context.Context, upstream *socket.Connector, command *pb.WorldCommand) ([]byte, error) {
	client, err := connectedWorldClient(upstream)
	if err != nil {
		return nil, err
	}
	// 内网保留游戏服的 JSON 数据格式；Orbit 在独立读循环里按序号匹配回应。
	body, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(command)
	if err != nil {
		return nil, err
	}
	response, err := client.Call(ctx, rpc.Frame{ID: worldRequestID, Payload: body}, worldReplyID)
	if err != nil {
		return nil, err
	}
	var reply pb.WorldReply
	if protojson.Unmarshal(response.Payload, &reply) != nil {
		return nil, errors.New("invalid internal world reply")
	}
	payload, err := proto.Marshal(&reply)
	if err != nil {
		return nil, err
	}
	if len(payload) == 0 || len(payload) > maxSessionPayload {
		return nil, errors.New("invalid world reply size")
	}
	return payload, nil
}
