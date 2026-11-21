package testcases

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NumberMan1/orbit/rpc"
	orbittcp "github.com/NumberMan1/orbit/socket/tcp"
	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/NumberMan1/slg-server/internal/gateway"
	pb "github.com/NumberMan1/slg-server/proto"
	"github.com/NumberMan1/slg-server/testkit"
)

func TestSharedWorldOverTCPAndWebSocket(t *testing.T) {
	t.Setenv("SLG_WORLD_IDLE_SECONDS", "60")
	game, stopGame := startCPPGameServer(t, "127.0.0.1:0")
	upstream, err := gateway.NewWorldConnector(game)
	if err != nil {
		t.Fatal(err)
	}
	if err := upstream.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := upstream.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	waitForUpstream := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if _, err := upstream.WaitSession(ctx); err != nil {
			t.Fatal(err)
		}
	}
	waitForUpstream()
	if err := gateway.CheckWorldReady(t.Context(), upstream); err != nil {
		t.Fatalf("Orbit readiness failed after connecting: %v", err)
	}
	tickets := &memoryTickets{}
	handle := gateway.NewSessions(tickets).WorldHandler(upstream)
	options, err := gateway.SessionOptions()
	if err != nil {
		t.Fatal(err)
	}
	tcp, err := orbittcp.NewServer("127.0.0.1:0", options, handle, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tcp.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	web, err := gateway.NewWebSessions(options, handle)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := tcp.Stop(ctx); err != nil {
			t.Error(err)
		}
		if err := web.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	webURL, _ := testkit.StartHTTP(t, gateway.WithWeb(http.NotFoundHandler(), web, ""))

	connectTCP := func() net.Conn {
		t.Helper()
		conn, err := net.DialTimeout("tcp", tcp.Addr().String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
			t.Fatal(err)
		}
		return conn
	}
	exchangeTCP := func(conn net.Conn) worldExchange {
		return func(frame rpc.Frame) (rpc.Frame, error) {
			if err := rpc.Write(conn, frame, options.MaxPayload); err != nil {
				return rpc.Frame{}, err
			}
			return rpc.Read(conn, options.MaxPayload)
		}
	}
	reqID := uint16(pb.WorldMessageId_WORLD_MESSAGE_ID_REQUEST)
	if _, err := exchangeTCP(connectTCP())(rpc.Frame{ID: reqID, Sequence: 1}); err != io.EOF {
		t.Fatalf("unauthenticated world query: %v", err)
	}

	aConn := connectTCP()
	a := exchangeTCP(aConn)
	authenticateWorld(t, tickets, a, "player-a")
	bConn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(webURL, "http")+"/v1/session",
		http.Header{"Origin": []string{webURL}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bConn.Close() })
	if err := bConn.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}
	b := func(frame rpc.Frame) (rpc.Frame, error) {
		var encoded bytes.Buffer
		if err := rpc.Write(&encoded, frame, options.MaxPayload); err != nil {
			return rpc.Frame{}, err
		}
		if err := bConn.WriteMessage(websocket.BinaryMessage, encoded.Bytes()); err != nil {
			return rpc.Frame{}, err
		}
		_, payload, err := bConn.ReadMessage()
		if err != nil {
			return rpc.Frame{}, err
		}
		return rpc.Read(bytes.NewReader(payload), options.MaxPayload)
	}
	authenticateWorld(t, tickets, b, "player-b")

	// 两个不同传输上的账号进入同一张地图，并获得不同的出生地。
	aState := queryWorld(t, a, &pb.WorldRequest{}).GetSnapshot()
	bState := queryWorld(t, b, &pb.WorldRequest{}).GetSnapshot()
	if aState.GetYourPlayer() != 1 || bState.GetYourPlayer() != 2 ||
		aState.GetInstance() == "" || aState.GetInstance() != bState.GetInstance() ||
		len(bState.GetTiles()) != 19 {
		t.Fatalf("invalid shared world: %v / %v", aState, bState)
	}
	if unchanged := queryWorld(t, b, knownWorld(bState)); unchanged.GetSnapshot() != nil {
		t.Fatal("unchanged revision returned a full snapshot")
	}
	target := tileAt(bState, -1, 0)
	if target == nil || target.GetOwner() != 0 {
		t.Fatal("expected neutral tile next to player A's home")
	}
	order := knownWorld(aState)
	order.Order = &pb.MarchOrder{CommandId: "test-march-a", TargetTileId: target.GetId()}
	started := queryWorld(t, a, order)
	if started.GetResult() != pb.WorldResult_WORLD_RESULT_OK || len(started.GetSnapshot().GetMarches()) != 1 {
		t.Fatalf("march did not start: %v", started)
	}
	bMarching := queryWorld(t, b, knownWorld(bState)).GetSnapshot()
	if len(bMarching.GetMarches()) != 1 || bMarching.GetMarches()[0].GetTargetTileId() != target.GetId() {
		t.Fatal("player B did not see player A's march")
	}
	_ = aConn.Close()
	saved := started.GetSnapshot()
	reconnected := exchangeTCP(connectTCP())
	authenticateWorld(t, tickets, reconnected, "player-a")

	// 等待真实进程结算，再用原指令重试，确认重连不会再次派兵。
	deadline := time.Now().Add(5 * time.Second)
	for {
		updated := queryWorld(t, b, knownWorld(bMarching)).GetSnapshot()
		if updated != nil && tileAt(updated, -1, 0).GetOwner() == 1 && len(updated.GetMarches()) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("player B did not see arrival and capture")
		}
		time.Sleep(75 * time.Millisecond)
	}
	order.KnownRevision = saved.GetRevision()
	replayed := queryWorld(t, reconnected, order)
	if replayed.GetResult() != pb.WorldResult_WORLD_RESULT_OK ||
		replayed.GetSnapshot().GetYourPlayer() != 1 ||
		tileAt(replayed.GetSnapshot(), -1, 0).GetOwner() != 1 ||
		len(replayed.GetSnapshot().GetMarches()) != 0 {
		t.Fatalf("reconnect replay changed the outcome: %v", replayed)
	}

	// 游戏服断线时保留前端会话；新游戏进程启动后，网关重建内网请求连接。
	stopGame()
	if err := gateway.CheckWorldReady(t.Context(), upstream); err == nil {
		t.Fatal("Orbit reported ready after the game server stopped")
	}
	if unavailable := queryWorld(t, b, knownWorld(bMarching)); unavailable.GetResult() != pb.WorldResult_WORLD_RESULT_UNAVAILABLE {
		t.Fatalf("world should be unavailable while the C++ process is stopped: %v", unavailable)
	}
	startCPPGameServer(t, game)
	waitForUpstream()
	restarted := queryWorld(t, b, knownWorld(bMarching)).GetSnapshot()
	if restarted.GetInstance() == "" || restarted.GetInstance() == bMarching.GetInstance() {
		t.Fatalf("expected fresh world after upstream reconnect: %v", restarted)
	}
}

func TestWorldReclaimsExpiredSeat(t *testing.T) {
	t.Setenv("SLG_WORLD_IDLE_SECONDS", "2")
	address, _ := startCPPGameServer(t, "127.0.0.1:0")
	connection, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var sequence uint32
	world := func(account string, request *pb.WorldRequest) *pb.WorldReply {
		t.Helper()
		if request == nil {
			request = &pb.WorldRequest{}
		}
		data, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(&pb.WorldCommand{Account: account, Request: request})
		if err != nil {
			t.Fatal(err)
		}
		sequence++
		if err := rpc.Write(connection, rpc.Frame{ID: uint16(pb.WorldMessageId_WORLD_MESSAGE_ID_REQUEST), Sequence: sequence, Payload: data}, 4096); err != nil {
			t.Fatal(err)
		}
		frame, err := rpc.Read(connection, 4096)
		var reply pb.WorldReply
		if err != nil || frame.Sequence != sequence || frame.ID != uint16(pb.WorldMessageId_WORLD_MESSAGE_ID_REPLY) ||
			protojson.Unmarshal(frame.Payload, &reply) != nil {
			t.Fatalf("invalid world reply: %v, %v", frame, err)
		}
		return &reply
	}
	a := world("active-player", nil).GetSnapshot()
	b := world("idle-player", nil).GetSnapshot()
	if a.GetYourPlayer() != 1 || b.GetYourPlayer() != 2 || a.GetInstance() != b.GetInstance() {
		t.Fatal("players did not receive two seats in the same world")
	}
	if got := world("waiting-player", nil).GetResult(); got != pb.WorldResult_WORLD_RESULT_FULL {
		t.Fatalf("world should keep both active seats: %v", got)
	}
	order := knownWorld(b)
	order.Order = &pb.MarchOrder{CommandId: "unsettled-order", TargetTileId: tileAt(b, 1, 0).GetId()}
	if result := world("idle-player", order); result.GetResult() != pb.WorldResult_WORLD_RESULT_OK || len(result.GetSnapshot().GetMarches()) != 1 {
		t.Fatalf("could not start idle player's march: %v", result)
	}
	// 持续刷新一号玩家，二号玩家在离线宽限期后释放尚未到达的行军和出生地。
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := world("active-player", nil).GetSnapshot().GetYourPlayer(); got != 1 {
			t.Fatalf("active player lost its seat: %v", got)
		}
		if c := world("waiting-player", nil).GetSnapshot(); c != nil {
			if c.GetYourPlayer() != 2 || c.GetInstance() == a.GetInstance() ||
				tileAt(c, -2, 0).GetOwner() != 1 || tileAt(c, 1, 0).GetOwner() != 0 || len(c.GetMarches()) != 0 {
				t.Fatalf("invalid world after reclaiming the seat: %v", c)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle player did not release its seat")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if result := world("idle-player", order).GetResult(); result != pb.WorldResult_WORLD_RESULT_WORLD_CHANGED {
		t.Fatalf("the expired world's order was not rejected: %v", result)
	}
}

type worldExchange func(rpc.Frame) (rpc.Frame, error)

func authenticateWorld(t *testing.T, tickets *memoryTickets, send worldExchange, account string) {
	t.Helper()
	ticket, err := tickets.Issue(t.Context(), account)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := proto.Marshal(&pb.ConnectRequest{Ticket: ticket, ProtocolVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := send(rpc.Frame{ID: uint16(pb.SessionMessageId_SESSION_MESSAGE_ID_CONNECT_REQUEST), Sequence: 1, Payload: payload})
	var reply pb.ConnectReply
	if err != nil || frame.ID != uint16(pb.SessionMessageId_SESSION_MESSAGE_ID_CONNECT_REPLY) ||
		proto.Unmarshal(frame.Payload, &reply) != nil || reply.GetAccount() != account {
		t.Fatalf("invalid session reply: %v, %v", frame, err)
	}
}

func queryWorld(t *testing.T, send worldExchange, request *pb.WorldRequest) *pb.WorldReply {
	t.Helper()
	payload, err := proto.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := send(rpc.Frame{ID: uint16(pb.WorldMessageId_WORLD_MESSAGE_ID_REQUEST), Sequence: 2, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	var reply pb.WorldReply
	if frame.ID != uint16(pb.WorldMessageId_WORLD_MESSAGE_ID_REPLY) || frame.Sequence != 2 ||
		proto.Unmarshal(frame.Payload, &reply) != nil {
		t.Fatalf("invalid world reply: %v", frame)
	}
	return &reply
}

func knownWorld(snapshot *pb.WorldSnapshot) *pb.WorldRequest {
	return &pb.WorldRequest{KnownInstance: snapshot.GetInstance(), KnownRevision: snapshot.GetRevision()}
}

func tileAt(snapshot *pb.WorldSnapshot, q, r int32) *pb.WorldTile {
	for _, tile := range snapshot.GetTiles() {
		if tile.GetQ() == q && tile.GetR() == r {
			return tile
		}
	}
	return nil
}

func startCPPGameServer(t *testing.T, address string) (string, func()) {
	t.Helper()
	binary := os.Getenv("SLG_TEST_GAME_SERVER")
	if binary == "" {
		t.Skip("run scripts/verify.sh to build the C++ game server for this test")
	}
	process := exec.CommandContext(t.Context(), binary)
	process.Env = append(process.Environ(), "SLG_GAME_ADDR=127.0.0.1:0", "SLG_GAME_RPC_ADDR="+address)
	process.Stderr = os.Stderr
	output, err := process.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	stop := sync.OnceFunc(func() {
		_ = process.Process.Kill()
		_ = process.Wait()
	})
	t.Cleanup(stop)
	ready := make(chan string, 1)
	go func() {
		lines := bufio.NewScanner(output)
		if lines.Scan() {
			ready <- lines.Text()
		}
		close(ready)
	}()
	select {
	case line := <-ready:
		if line == "" {
			t.Fatalf("C++ game server exited before listening: %v", process.Wait())
		}
		if addr, ok := strings.CutPrefix(line, "game-server rpc listening "); ok {
			return addr, stop
		}
		t.Fatalf("unexpected game server startup output: %q", line)
	case <-time.After(3 * time.Second):
		t.Fatal("C++ game server did not start")
	}
	return "", nil
}
