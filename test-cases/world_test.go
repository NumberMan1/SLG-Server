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
	battle, stopBattle := startCPPServer(t, "SLG_TEST_BATTLE_SERVER", "battle-server rpc listening ", "SLG_BATTLE_RPC_ADDR=127.0.0.1:0")
	_, battlePort, err := net.SplitHostPort(battle)
	if err != nil {
		t.Fatal(err)
	}
	// 监听仍用数字地址，游戏服用主机名连接，覆盖 Podman 的容器 DNS 用法。
	t.Setenv("SLG_BATTLE_RPC_UPSTREAM", net.JoinHostPort("localhost", battlePort))
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
	waitForBattleReadiness := func(ready bool) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
		defer cancel()
		for {
			err := gateway.CheckWorldReady(ctx, upstream)
			if (err == nil) == ready {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatalf("battle readiness did not become %v: %v", ready, err)
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	waitForBattleReadiness(true)
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
		if err := conn.SetDeadline(time.Now().Add(35 * time.Second)); err != nil {
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
	if err := bConn.SetReadDeadline(time.Now().Add(35 * time.Second)); err != nil {
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
	issueMarch := func(command string, q int32, troops uint32) *pb.WorldSnapshot {
		t.Helper()
		current := queryWorld(t, b, &pb.WorldRequest{}).GetSnapshot()
		request := knownWorld(current)
		request.Order = &pb.MarchOrder{CommandId: command, TargetTileId: tileAt(current, q, 0).GetId(), Troops: troops}
		response := queryWorld(t, b, request)
		if response.GetResult() != pb.WorldResult_WORLD_RESULT_OK || len(response.GetSnapshot().GetMarches()) != 1 {
			t.Fatalf("failed to send %s: %v", command, response)
		}
		return response.GetSnapshot()
	}
	waitForWorld := func(expect func(*pb.WorldReply) bool) *pb.WorldSnapshot {
		t.Helper()
		deadline := time.Now().Add(6 * time.Second)
		for {
			reply := queryWorld(t, b, &pb.WorldRequest{})
			if expect(reply) {
				return reply.GetSnapshot()
			}
			if time.Now().After(deadline) {
				t.Fatalf("world did not reach the expected state: %v", reply)
			}
			time.Sleep(80 * time.Millisecond)
		}
	}
	for _, crossing := range []struct {
		q  int32
		id string
	}{{1, "right-border"}, {0, "middle-border"}} {
		issueMarch(crossing.id, crossing.q, 20)
		waitForWorld(func(reply *pb.WorldReply) bool {
			tile := tileAt(reply.GetSnapshot(), crossing.q, 0)
			return tile.GetOwner() == 2 && tile.GetGarrison() == 20
		})
	}
	firstAttack := issueMarch("first-attack", -1, 20).GetMarches()[0]
	if !firstAttack.GetHostile() || firstAttack.GetTroops() != 20 {
		t.Fatalf("missing battle march: %v", firstAttack)
	}
	stopBattle()
	waitForBattleReadiness(false)
	// 战斗服不可用时前端仍然能读取地图，到达的部队等待原快照结算。
	waitForWorld(func(reply *pb.WorldReply) bool { return reply.GetServerTimeMs() > firstAttack.GetArrivesAtMs()+200 })
	awaiting := queryWorld(t, b, &pb.WorldRequest{}).GetSnapshot()
	if tileAt(awaiting, -1, 0).GetOwner() != 1 || awaiting.GetLastBattle() != nil || len(awaiting.GetMarches()) != 1 {
		t.Fatalf("pending combat changed the map: %v", awaiting)
	}
	startCPPServer(t, "SLG_TEST_BATTLE_SERVER", "battle-server rpc listening ", "SLG_BATTLE_RPC_ADDR="+battle)
	waitForBattleReadiness(true)
	defended := waitForWorld(func(reply *pb.WorldReply) bool { return reply.GetSnapshot().GetLastBattle() != nil })
	defense := defended.GetLastBattle()
	if defense.GetCaptured() || defense.GetAttackerTroops() != 20 || defense.GetDefenderTroops() != 40 ||
		defense.GetDefenderSurvivors() != 30 || tileAt(defended, -1, 0).GetOwner() != 1 || tileAt(defended, -1, 0).GetGarrison() != 30 {
		t.Fatalf("defense was not applied correctly: %v", defended)
	}
	_ = queryWorld(t, reconnected, &pb.WorldRequest{})
	issueMarch("capture-attack", -1, 60)
	captured := waitForWorld(func(reply *pb.WorldReply) bool {
		return reply.GetSnapshot().GetLastBattle().GetBattleId() > defense.GetBattleId()
	})
	shared := queryWorld(t, reconnected, &pb.WorldRequest{}).GetSnapshot()
	if !captured.GetLastBattle().GetCaptured() || tileAt(captured, -1, 0).GetOwner() != 2 ||
		tileAt(captured, -1, 0).GetGarrison() != 45 || !proto.Equal(captured.GetLastBattle(), shared.GetLastBattle()) {
		t.Fatalf("the players did not see the same victorious settlement: %v / %v", captured, shared)
	}
	// 新客户端经网关读取游标之后的战报和独立的兵力更新。
	incremental := knownWorld(captured)
	after := defense.GetBattleId()
	incremental.KnownBattleId = &after
	updated := queryWorld(t, b, incremental)
	if updated.AvailableTroops == nil || len(updated.GetBattleReports()) != 1 ||
		!proto.Equal(updated.GetBattleReports()[0], captured.GetLastBattle()) {
		t.Fatalf("gateway did not forward incremental world state: %v", updated)
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
	world := directWorld(t, address)
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

func TestSlowBattleProbeDoesNotBlockWorld(t *testing.T) {
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("SLG_BATTLE_RPC_UPSTREAM", listener.Addr().String())
	address, _ := startCPPGameServer(t, "127.0.0.1:0")
	if err := listener.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// 接收后台探测的连接但不回包，验证应用还能立即响应请求。
	stalled, err := listener.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()
	upstream, err := gateway.NewWorldConnector(address)
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
	connectCtx, cancelConnect := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancelConnect()
	if _, err := upstream.WaitSession(connectCtx); err != nil {
		t.Fatal(err)
	}
	probeCtx, cancelProbe := context.WithTimeout(t.Context(), 350*time.Millisecond)
	defer cancelProbe()
	if err := gateway.CheckWorldReady(probeCtx, upstream); err == nil || probeCtx.Err() != nil {
		t.Fatalf("expected immediate not-ready response while battle probe is blocked: %v", err)
	}
	if reply := directWorld(t, address)("active-player", nil); reply.GetSnapshot().GetYourPlayer() != 1 {
		t.Fatalf("world was unavailable while the battle probe was blocked: %v", reply)
	}
}

func TestPendingBattlesKeepSeatsAndReturnSurvivors(t *testing.T) {
	t.Setenv("SLG_WORLD_IDLE_SECONDS", "6")
	battle, stopBattle := startCPPServer(t, "SLG_TEST_BATTLE_SERVER", "battle-server rpc listening ", "SLG_BATTLE_RPC_ADDR=127.0.0.1:0")
	t.Setenv("SLG_BATTLE_RPC_UPSTREAM", battle)
	address, _ := startCPPGameServer(t, "127.0.0.1:0")
	world := directWorld(t, address)
	const attacker, defender, waiting = "attacker", "defender", "waiting"
	syncWorld := func(snapshot *pb.WorldSnapshot, after uint64) *pb.WorldRequest {
		request := knownWorld(snapshot)
		request.KnownBattleId = &after
		return request
	}
	read := func(account string) *pb.WorldReply {
		return world(account, syncWorld(nil, 0))
	}
	order := func(account, id string, q int32, troops uint32) *pb.WorldReply {
		t.Helper()
		current := read(account).GetSnapshot()
		request := syncWorld(current, 0)
		request.Order = &pb.MarchOrder{CommandId: id, TargetTileId: tileAt(current, q, 0).GetId(), Troops: troops}
		reply := world(account, request)
		if reply.GetResult() != pb.WorldResult_WORLD_RESULT_OK {
			t.Fatalf("failed to place %s: %v", id, reply)
		}
		return reply
	}
	waitFor := func(account string, expect func(*pb.WorldReply) bool) *pb.WorldReply {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for {
			reply := read(account)
			if expect(reply) {
				return reply
			}
			if time.Now().After(deadline) {
				t.Fatalf("world did not reach expected state: %v", reply)
			}
			time.Sleep(60 * time.Millisecond)
		}
	}

	order(attacker, "west", -1, 2)
	order(defender, "east", 1, 1)
	waitFor(attacker, func(reply *pb.WorldReply) bool { return tileAt(reply.GetSnapshot(), 1, 0).GetOwner() == 2 })
	center := order(defender, "center", 0, 20)
	current := read(attacker).GetSnapshot()
	waitFor(defender, func(reply *pb.WorldReply) bool { return reply.GetAvailableTroops() > center.GetAvailableTroops() })
	// 只补充二号玩家的私有兵力时，一号玩家仍沿用原来的地图快照。
	if reply := world(attacker, syncWorld(current, 0)); reply.GetSnapshot() != nil || reply.GetAvailableTroops() != 100 {
		t.Fatalf("private troop update invalidated the other player's map: %v", reply)
	}
	waitFor(attacker, func(reply *pb.WorldReply) bool { return tileAt(reply.GetSnapshot(), 0, 0).GetOwner() == 2 })

	stopBattle()
	order(attacker, "attack-center", 0, 16)
	counterattack := order(defender, "attack-west", -1, 1)
	// 暂停战斗服直到二号玩家超过空闲时限。双方敌对行军与驻军仍应保留。
	pending := waitFor(attacker, func(reply *pb.WorldReply) bool {
		return reply.GetServerTimeMs() > counterattack.GetServerTimeMs()+6200
	})
	if tileAt(pending.GetSnapshot(), 0, 0).GetOwner() != 2 || len(pending.GetSnapshot().GetMarches()) != 2 ||
		len(pending.GetBattleReports()) != 0 || read(waiting).GetResult() != pb.WorldResult_WORLD_RESULT_FULL {
		t.Fatalf("pending battles failed to keep their seats and garrisons: %v", pending)
	}
	startCPPServer(t, "SLG_TEST_BATTLE_SERVER", "battle-server rpc listening ", "SLG_BATTLE_RPC_ADDR="+battle)
	settled := waitFor(attacker, func(reply *pb.WorldReply) bool { return len(reply.GetBattleReports()) == 2 })
	reports := settled.GetBattleReports()
	var retreat *pb.WorldBattleReport
	for _, report := range reports {
		if report.GetAttacker() == 1 {
			retreat = report
		}
	}
	if reports[0].GetBattleId() >= reports[1].GetBattleId() || retreat.GetCaptured() ||
		retreat.GetDefender() != 2 || retreat.GetAttackerSurvivors() != 6 || settled.GetAvailableTroops() != 106 {
		t.Fatalf("ordered reports or overflow troops were lost: %v", settled)
	}
	rejoined := read(waiting)
	if rejoined.GetSnapshot().GetYourPlayer() != 2 || len(rejoined.GetBattleReports()) != 2 {
		t.Fatalf("expected a free seat and both reports after settlement: %v", rejoined)
	}
	time.Sleep(1100 * time.Millisecond)
	latest := read(attacker)
	delta := world(attacker, syncWorld(latest.GetSnapshot(), reports[0].GetBattleId()))
	if delta.GetSnapshot() != nil || delta.GetAvailableTroops() != 106 || len(delta.GetBattleReports()) != 1 ||
		!proto.Equal(delta.GetBattleReports()[0], reports[1]) {
		t.Fatalf("overflow reserves or report cursor incorrect: %v", delta)
	}
	dispatched := order(attacker, "spend-overflow", 0, 7)
	if dispatched.GetAvailableTroops() != 99 {
		t.Fatalf("overflow reserves were not available to dispatch: %v", dispatched)
	}
	waitFor(attacker, func(reply *pb.WorldReply) bool { return reply.GetAvailableTroops() == 100 })
}

func directWorld(t *testing.T, address string) func(string, *pb.WorldRequest) *pb.WorldReply {
	t.Helper()
	connection, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	var sequence uint32
	return func(account string, request *pb.WorldRequest) *pb.WorldReply {
		t.Helper()
		if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
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
	return startCPPServer(t, "SLG_TEST_GAME_SERVER", "game-server rpc listening ", "SLG_GAME_ADDR=127.0.0.1:0", "SLG_GAME_RPC_ADDR="+address)
}

func startCPPServer(t *testing.T, binaryEnv, readyPrefix string, settings ...string) (string, func()) {
	t.Helper()
	binary := os.Getenv(binaryEnv)
	if binary == "" {
		t.Skipf("run scripts/verify.sh to set %s", binaryEnv)
	}
	process := exec.CommandContext(t.Context(), binary)
	process.Env = append(process.Environ(), settings...)
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
			t.Fatalf("%s exited before listening: %v", binaryEnv, process.Wait())
		}
		if addr, ok := strings.CutPrefix(line, readyPrefix); ok {
			return addr, stop
		}
		t.Fatalf("unexpected %s startup output: %q", binaryEnv, line)
	case <-time.After(3 * time.Second):
		t.Fatalf("%s did not start", binaryEnv)
	}
	return "", nil
}
