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
	"github.com/NumberMan1/orbit/socket"
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
	game, stopGame := startCPPGameServer(t, "127.0.0.1:0", "SLG_WORLD_RADIUS=7")
	upstream, waitForUpstream := startWorldUpstream(t, game)
	tickets := &memoryTickets{}
	handle := gateway.NewSessions(tickets).WorldHandler(upstream)
	options, err := gateway.SessionOptions()
	if err != nil {
		t.Fatal(err)
	}
	tcp := startWorldTCP(t, options, handle)
	web, err := gateway.NewWebSessions(options, handle)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
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
	reqID := uint16(pb.WorldMessageId_WORLD_MESSAGE_ID_REQUEST)
	if _, err := exchangeWorldTCP(connectTCP(), options.MaxPayload)(rpc.Frame{ID: reqID, Sequence: 1}); err != io.EOF {
		t.Fatalf("unauthenticated world query: %v", err)
	}

	aConn := connectTCP()
	a := exchangeWorldTCP(aConn, options.MaxPayload)
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
	bState := queryWorld(t, b, &pb.WorldRequest{KnownTileRevision: proto.Uint64(0)}).GetSnapshot()
	if aState.GetYourPlayer() != 1 || bState.GetYourPlayer() != 2 ||
		aState.GetInstance() == "" || aState.GetInstance() != bState.GetInstance() ||
		len(bState.GetTiles()) != 169 || len(aState.GetTiles()) != 169 || bState.GetTileRevision() == 0 || aState.TileRevision != nil {
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
	bDelta := knownWorld(bState)
	bDelta.KnownTileRevision = bState.TileRevision
	bMarching := queryWorld(t, b, bDelta).GetSnapshot()
	if len(bMarching.GetMarches()) != 1 || bMarching.GetMarches()[0].GetTargetTileId() != target.GetId() ||
		len(bMarching.GetTiles()) != 0 || bMarching.GetTileRevision() != bState.GetTileRevision() {
		t.Fatalf("player B should receive the march and keep cached tiles: %v", bMarching)
	}
	_ = aConn.Close()
	saved := started.GetSnapshot()
	reconnected := exchangeWorldTCP(connectTCP(), options.MaxPayload)
	authenticateWorld(t, tickets, reconnected, "player-a")

	// 等待真实进程结算，再用原指令重试，确认重连不会再次派兵。
	deadline := time.Now().Add(5 * time.Second)
	bDelta.KnownRevision = bMarching.GetRevision()
	for {
		updated := queryWorld(t, b, bDelta).GetSnapshot()
		if updated != nil && tileAt(updated, -1, 0).GetOwner() == 1 && len(updated.GetMarches()) == 0 {
			if len(updated.GetTiles()) != 169 || updated.GetTileRevision() == bState.GetTileRevision() {
				t.Fatalf("capture should refresh tile data: %v", updated)
			}
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

func TestSharedNeutralTileSettlesInArrivalOrder(t *testing.T) {
	t.Setenv("SLG_WORLD_IDLE_SECONDS", "60")
	address, _ := startCPPGameServer(t, "127.0.0.1:0")
	world := directWorld(t, address)
	const west, east = "west", "east"
	read := func(account string) *pb.WorldSnapshot { return world(account, nil).GetSnapshot() }
	send := func(account, command string, target *pb.WorldTile, troops uint32, snapshot *pb.WorldSnapshot) *pb.WorldSnapshot {
		t.Helper()
		request := knownWorld(snapshot)
		request.Order = &pb.MarchOrder{CommandId: command, TargetTileId: target.GetId(), Troops: troops}
		reply := world(account, request)
		if reply.GetResult() != pb.WorldResult_WORLD_RESULT_OK || reply.GetSnapshot() == nil {
			t.Fatalf("failed to send %s: %v", command, reply)
		}
		return reply.GetSnapshot()
	}
	waitFor := func(expect func(*pb.WorldSnapshot) bool) *pb.WorldSnapshot {
		t.Helper()
		deadline := time.Now().Add(7 * time.Second)
		for {
			snapshot := read(west)
			if expect(snapshot) {
				return snapshot
			}
			if time.Now().After(deadline) {
				t.Fatalf("world did not reach the expected state: %v", snapshot)
			}
			time.Sleep(60 * time.Millisecond)
		}
	}
	a, b := read(west), read(east)
	send(west, "west-border", tileAt(a, -1, 0), 20, a)
	send(east, "east-border", tileAt(b, 1, 0), 20, b)
	ready := waitFor(func(snapshot *pb.WorldSnapshot) bool {
		return tileAt(snapshot, -1, 0).GetOwner() == 1 && tileAt(snapshot, 1, 0).GetOwner() == 2
	})
	center := tileAt(ready, 0, 0)
	// 二号玩家先出发。一号玩家沿用开战前的快照，模拟同时点击同一块中立地。
	first := send(east, "east-center", center, 20, ready)
	if len(first.GetMarches()) != 1 || first.GetMarches()[0].GetPlayer() != 2 {
		t.Fatalf("first march was not registered: %v", first)
	}
	contested := send(west, "west-center", center, 60, ready)
	if len(contested.GetMarches()) != 2 || tileAt(contested, 0, 0).GetOwner() != 0 {
		t.Fatalf("both players should be able to march to a neutral tile: %v", contested)
	}
	for _, march := range contested.GetMarches() {
		if march.GetTargetTileId() != center.GetId() || march.GetHostile() {
			t.Fatalf("unexpected march before the first arrival: %v", march)
		}
	}
	settled := waitFor(func(snapshot *pb.WorldSnapshot) bool { return snapshot.GetLastBattle() != nil })
	report := settled.GetLastBattle()
	if report.GetTargetTileId() != center.GetId() || report.GetAttacker() != 1 || report.GetDefender() != 2 ||
		report.GetAttackerTroops() != 60 || report.GetDefenderTroops() != 20 || !report.GetCaptured() ||
		tileAt(settled, 0, 0).GetGarrison() != 50 || len(settled.GetMarches()) != 0 ||
		!proto.Equal(report, read(east).GetLastBattle()) {
		t.Fatalf("the late arrival did not settle one shared battle against the first arrival: %v", settled)
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

func TestBattleReturnsSurvivorsAndKeepsPrivateReserveUpdates(t *testing.T) {
	t.Setenv("SLG_WORLD_IDLE_SECONDS", "60")
	address, _ := startCPPGameServer(t, "127.0.0.1:0")
	world := directWorld(t, address)
	const attacker, defender = "attacker", "defender"
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

	order(attacker, "attack-center", 0, 16)
	settled := waitFor(attacker, func(reply *pb.WorldReply) bool { return len(reply.GetBattleReports()) == 1 })
	report := settled.GetBattleReports()[0]
	overflow := settled.GetAvailableTroops()
	if report.GetCaptured() || report.GetDefender() != 2 || report.GetAttackerSurvivors() != 6 ||
		overflow < 105 || overflow > 106 {
		t.Fatalf("local settlement did not return the survivors: %v", settled)
	}
	time.Sleep(1100 * time.Millisecond)
	latest := read(attacker)
	delta := world(attacker, syncWorld(latest.GetSnapshot(), report.GetBattleId()))
	if delta.GetSnapshot() != nil || delta.GetAvailableTroops() != overflow || len(delta.GetBattleReports()) != 0 {
		t.Fatalf("overflow reserves or report cursor incorrect: %v", delta)
	}
	dispatched := order(attacker, "spend-overflow", 0, overflow-99)
	if dispatched.GetAvailableTroops() != 99 {
		t.Fatalf("overflow reserves were not available to dispatch: %v", dispatched)
	}
	waitFor(attacker, func(reply *pb.WorldReply) bool { return reply.GetAvailableTroops() == 100 })
}

func TestBattleRoundReinforcements(t *testing.T) {
	t.Setenv("SLG_WORLD_IDLE_SECONDS", "5")
	address, _ := startCPPGameServer(t, "127.0.0.1:0")
	world := directWorld(t, address)
	const west, east = "west", "east"
	read := func(account string) *pb.WorldReply { return world(account, nil) }
	makeOrder := func(account, command string, q int32, troops uint32) *pb.WorldRequest {
		snapshot := read(account).GetSnapshot()
		request := knownWorld(snapshot)
		request.Order = &pb.MarchOrder{CommandId: command, TargetTileId: tileAt(snapshot, q, 0).GetId(), Troops: troops}
		return request
	}
	send := func(account string, request *pb.WorldRequest) *pb.WorldReply {
		t.Helper()
		reply := world(account, request)
		if reply.GetResult() != pb.WorldResult_WORLD_RESULT_OK {
			t.Fatalf("failed to send %s: %v", request.GetOrder().GetCommandId(), reply)
		}
		return reply
	}
	waitFor := func(expect func(*pb.WorldReply) bool) *pb.WorldReply {
		t.Helper()
		deadline := time.Now().Add(9 * time.Second)
		for {
			reply := read(west)
			if expect(reply) {
				return reply
			}
			if time.Now().After(deadline) {
				t.Fatalf("world did not reach the expected state: %v", reply)
			}
			time.Sleep(45 * time.Millisecond)
		}
	}
	live := func(reply *pb.WorldReply) *pb.WorldBattle {
		if battles := reply.GetSnapshot().GetBattles(); len(battles) == 1 {
			return battles[0]
		}
		return nil
	}
	read(west)
	read(east)
	send(west, makeOrder(west, "west-border", -1, 20))
	send(east, makeOrder(east, "east-border", 1, 20))
	waitFor(func(reply *pb.WorldReply) bool {
		return tileAt(reply.GetSnapshot(), -1, 0).GetOwner() == 1 && tileAt(reply.GetSnapshot(), 1, 0).GetOwner() == 2
	})
	read(east)
	send(west, makeOrder(west, "defend-center", 0, 40))
	waitFor(func(reply *pb.WorldReply) bool { return tileAt(reply.GetSnapshot(), 0, 0).GetOwner() == 1 })
	enter := makeOrder(east, "attack-center", 0, 40)
	enter.Order.RoundBattle = true
	send(east, enter)
	started := waitFor(func(reply *pb.WorldReply) bool { return live(reply).GetRound() == 1 })
	first := live(started)
	if first.GetRounds() != 3 || first.GetAttackerTroops() != 40 || first.GetDefenderTroops() != 40 {
		t.Fatalf("first round did not freeze both forces: %v", first)
	}
	if remaining := first.GetRoundEndsAtMs() - started.GetServerTimeMs(); remaining < 5000 || remaining > 6000 {
		t.Fatalf("an initial round must leave time to command a three-second march: %d ms", remaining)
	}
	wests := makeOrder(west, "west-support", 0, 20)
	easts := makeOrder(east, "east-support", 0, 20)
	wests.Order.ReinforceBattleId, easts.Order.ReinforceBattleId = first.GetBattleId(), first.GetBattleId()
	send(west, wests)
	if marches := send(east, easts).GetSnapshot().GetMarches(); len(marches) != 2 ||
		marches[0].GetReinforceBattleId() != first.GetBattleId() || marches[1].GetReinforceBattleId() != first.GetBattleId() {
		t.Fatalf("both reinforcement routes were not present: %v", marches)
	}
	send(east, easts)
	waiting := live(waitFor(func(reply *pb.WorldReply) bool {
		return live(reply).GetAttackerWaiting() == 20 && live(reply).GetDefenderWaiting() == 20
	}))
	if waiting.GetRound() != 1 || waiting.GetAttackerTroops() != 40 || waiting.GetDefenderTroops() != 40 {
		t.Fatalf("reinforcements altered a running round: %v", waiting)
	}
	second := live(waitFor(func(reply *pb.WorldReply) bool { return live(reply).GetRound() == 2 }))
	if second.GetAttackerTroops() != 50 || second.GetDefenderTroops() != 50 || second.GetAttackerWaiting() != 0 || second.GetDefenderWaiting() != 0 {
		t.Fatalf("both reinforcements should join exactly once in round two: %v", second)
	}
	// 二号玩家已超过空闲时限，正在交战的双方仍保留席位。
	if reply := read("waiting"); reply.GetResult() != pb.WorldResult_WORLD_RESULT_FULL {
		t.Fatalf("an active battle should keep both seats: %v", reply)
	}
	waitFor(func(reply *pb.WorldReply) bool {
		battle := live(reply)
		return battle.GetRound() == 2 && battle.GetRoundEndsAtMs()-reply.GetServerTimeMs() < 1200
	})
	lateMarch := makeOrder(east, "late-support", 0, 20)
	lateMarch.Order.ReinforceBattleId = first.GetBattleId()
	debited := send(east, lateMarch).GetSnapshot().GetAvailableTroops()
	waitFor(func(reply *pb.WorldReply) bool { return live(reply).GetRound() == 3 })
	waitFor(func(reply *pb.WorldReply) bool { return len(reply.GetSnapshot().GetMarches()) == 0 })
	if returned := read(east).GetSnapshot().GetAvailableTroops(); returned < debited+20 {
		t.Fatalf("reinforcements that missed the decisive round should return: %d -> %d", debited, returned)
	}
	late := makeOrder(east, "too-late", 0, 20)
	late.Order.ReinforceBattleId = first.GetBattleId()
	if reply := world(east, late); reply.GetResult() != pb.WorldResult_WORLD_RESULT_REINFORCEMENT_CLOSED {
		t.Fatalf("the decisive round must reject new reinforcements: %v", reply)
	}
	settled := waitFor(func(reply *pb.WorldReply) bool { return reply.GetSnapshot().GetLastBattle() != nil })
	report := settled.GetSnapshot().GetLastBattle()
	if report.GetRounds() != 3 || report.GetAttackerReinforcements() != 20 || report.GetDefenderReinforcements() != 20 ||
		report.GetAttackerTroops() != 60 || report.GetDefenderTroops() != 60 || len(settled.GetSnapshot().GetBattles()) != 0 ||
		!proto.Equal(report, read(east).GetSnapshot().GetLastBattle()) {
		t.Fatalf("the players did not receive the same three-round report: %v", settled)
	}
}

func TestContinuousCombatReinforcementsAndRetreat(t *testing.T) {
	t.Setenv("SLG_WORLD_IDLE_SECONDS", "60")
	address, _ := startCPPGameServer(t, "127.0.0.1:0")
	world := directWorld(t, address)
	const west, east = "west", "east"
	read := func(account string) *pb.WorldSnapshot { return world(account, nil).GetSnapshot() }
	makeOrder := func(account, command string, q int32, troops uint32) *pb.WorldRequest {
		snapshot := read(account)
		request := knownWorld(snapshot)
		request.Order = &pb.MarchOrder{CommandId: command, TargetTileId: tileAt(snapshot, q, 0).GetId(), Troops: troops}
		return request
	}
	send := func(account string, request *pb.WorldRequest) {
		t.Helper()
		if reply := world(account, request); reply.GetResult() != pb.WorldResult_WORLD_RESULT_OK {
			t.Fatalf("failed to send %s: %v", request.GetOrder().GetCommandId(), reply)
		}
	}
	waitFor := func(expect func(*pb.WorldSnapshot) bool) *pb.WorldSnapshot {
		t.Helper()
		deadline := time.Now().Add(7 * time.Second)
		for {
			snapshot := read(west)
			if expect(snapshot) {
				return snapshot
			}
			if time.Now().After(deadline) {
				t.Fatalf("world did not reach the expected state: %v", snapshot)
			}
			time.Sleep(45 * time.Millisecond)
		}
	}
	live := func(snapshot *pb.WorldSnapshot) *pb.WorldBattle {
		if battles := snapshot.GetBattles(); len(battles) == 1 {
			return battles[0]
		}
		return nil
	}
	read(west)
	read(east)
	send(west, makeOrder(west, "west-border", -1, 20))
	send(east, makeOrder(east, "east-border", 1, 20))
	waitFor(func(snapshot *pb.WorldSnapshot) bool {
		return tileAt(snapshot, -1, 0).GetOwner() == 1 && tileAt(snapshot, 1, 0).GetOwner() == 2
	})
	send(east, makeOrder(east, "defend-center", 0, 60))
	waitFor(func(snapshot *pb.WorldSnapshot) bool { return tileAt(snapshot, 0, 0).GetOwner() == 2 })
	attack := makeOrder(west, "attack-center", 0, 60)
	attack.Order.ContinuousBattle = true
	send(west, attack)
	cached := world(west, &pb.WorldRequest{KnownTileRevision: proto.Uint64(0), KnownReportId: proto.Uint64(0)}).GetSnapshot()
	started := live(waitFor(func(snapshot *pb.WorldSnapshot) bool { return live(snapshot).GetRound() >= 2 }))
	if !started.GetContinuous() || started.GetRounds() != 0 || started.GetAttackerTroops() >= 60 || started.GetDefenderTroops() >= 60 {
		t.Fatalf("the first exchange should apply local losses without settling the battle: %v", started)
	}
	combatSync := knownWorld(cached)
	combatSync.KnownTileRevision = cached.TileRevision
	combatSync.KnownReportId = proto.Uint64(0)
	if delta := world(west, combatSync).GetSnapshot(); live(delta).GetRound() < started.GetRound() ||
		len(delta.GetTiles()) != 0 || delta.GetTileRevision() != cached.GetTileRevision() {
		t.Fatalf("ongoing rounds should update combat state without resending unchanged tiles: %v", delta)
	}
	wests := makeOrder(west, "west-support", 0, 40)
	easts := makeOrder(east, "east-support", 0, 40)
	wests.Order.ReinforceBattleId, easts.Order.ReinforceBattleId = started.GetBattleId(), started.GetBattleId()
	send(west, wests)
	send(east, easts)
	joined := live(waitFor(func(snapshot *pb.WorldSnapshot) bool { return len(snapshot.GetMarches()) == 0 }))
	if joined.GetRound() <= started.GetRound() || joined.GetAttackerTroops() <= started.GetAttackerTroops() ||
		joined.GetDefenderTroops() <= started.GetDefenderTroops() || joined.GetAttackerWaiting() != 0 || joined.GetDefenderWaiting() != 0 {
		t.Fatalf("arriving support should join while local exchanges continue: %v", joined)
	}
	send(east, easts)
	if len(read(east).GetMarches()) != 0 {
		t.Fatal("a replay sent additional support after arrival")
	}
	readReports := func(account string, cursor uint64) []*pb.WorldBattleReport {
		snapshot := read(account)
		request := knownWorld(snapshot)
		request.KnownReportId = &cursor
		return world(account, request).GetBattleReports()
	}
	otherAttack := makeOrder(east, "attack-west", -1, 20)
	otherAttack.Order.ContinuousBattle = true
	send(east, otherAttack)
	parallel := waitFor(func(snapshot *pb.WorldSnapshot) bool { return len(snapshot.GetBattles()) == 2 })
	other := parallel.GetBattles()[1]
	leaveOther := makeOrder(east, "leave-west", -1, 0)
	leaveOther.Order.RetreatBattleId = other.GetBattleId()
	send(east, leaveOther)
	firstReports := readReports(west, 0)
	if len(firstReports) != 1 || firstReports[0].GetBattleId() != other.GetBattleId() ||
		firstReports[0].GetRetreatedPlayer() != 2 || tileAt(read(west), -1, 0).GetOwner() != 1 {
		t.Fatalf("attacker retreat on the second battle should be visible while the first is live: %v", firstReports)
	}
	firstCursor := firstReports[0].GetReportId()

	late := makeOrder(west, "support-in-transit", 0, 20)
	late.Order.ReinforceBattleId = started.GetBattleId()
	send(west, late)
	debited := read(west).GetAvailableTroops()
	withdraw := makeOrder(east, "leave-center", 0, 0)
	withdraw.Order.RetreatBattleId = started.GetBattleId()
	send(east, withdraw)
	settled := read(west)
	finalReports, remoteReports := readReports(west, firstCursor), readReports(east, firstCursor)
	if len(finalReports) != 1 || len(remoteReports) != 1 {
		t.Fatalf("report cursor skipped the early battle: %v / %v", finalReports, remoteReports)
	}
	report := finalReports[0]
	if report.GetBattleId() != started.GetBattleId() || report.GetRetreatedPlayer() != 2 ||
		report.GetDefenderSurvivors() == 0 || report.GetAttackerReinforcements() != 40 ||
		report.GetDefenderReinforcements() != 40 || report.GetReportId() <= firstCursor || tileAt(settled, 0, 0).GetOwner() != 1 ||
		tileAt(settled, 0, 0).GetGarrison() != report.GetAttackerSurvivors() || len(settled.GetMarches()) != 0 ||
		settled.GetAvailableTroops() < debited+20 || !proto.Equal(report, remoteReports[0]) {
		t.Fatalf("defender retreat should end the battle and refund in-transit support: %v", settled)
	}
	send(east, withdraw)
	withdraw.Order.CommandId = "leave-closed-battle"
	if reply := world(east, withdraw); reply.GetResult() != pb.WorldResult_WORLD_RESULT_BATTLE_CLOSED {
		t.Fatalf("a new order against the closed battle should be rejected: %v", reply)
	}
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
		if err := rpc.Write(connection, rpc.Frame{ID: uint16(pb.WorldMessageId_WORLD_MESSAGE_ID_REQUEST), Sequence: sequence, Payload: data}, 32768); err != nil {
			t.Fatal(err)
		}
		frame, err := rpc.Read(connection, 32768)
		var reply pb.WorldReply
		if err != nil || frame.Sequence != sequence || frame.ID != uint16(pb.WorldMessageId_WORLD_MESSAGE_ID_REPLY) ||
			protojson.Unmarshal(frame.Payload, &reply) != nil {
			t.Fatalf("invalid world reply: %v, %v", frame, err)
		}
		return &reply
	}
}

type worldExchange func(rpc.Frame) (rpc.Frame, error)

func startWorldUpstream(t *testing.T, game string) (*socket.Connector, func()) {
	t.Helper()
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
	waitReady := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if _, err := upstream.WaitSession(ctx); err != nil {
			t.Fatal(err)
		}
		if err := gateway.CheckWorldReady(ctx, upstream); err != nil {
			t.Fatalf("world not ready: %v", err)
		}
	}
	waitReady()
	return upstream, waitReady
}

func startWorldTCP(t *testing.T, options socket.Options, handle socket.Handler) *orbittcp.Server {
	t.Helper()
	tcp, err := orbittcp.NewServer("127.0.0.1:0", options, handle, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tcp.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := tcp.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	return tcp
}

func exchangeWorldTCP(conn net.Conn, maxPayload int) worldExchange {
	return func(frame rpc.Frame) (rpc.Frame, error) {
		if err := rpc.Write(conn, frame, maxPayload); err != nil {
			return rpc.Frame{}, err
		}
		return rpc.Read(conn, maxPayload)
	}
}

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

func startCPPGameServer(t *testing.T, address string, extraEnv ...string) (string, func()) {
	t.Helper()
	const binaryEnv, readyPrefix = "SLG_TEST_GAME_SERVER", "game-server rpc listening "
	binary := os.Getenv(binaryEnv)
	if binary == "" {
		t.Skipf("run scripts/verify.sh to set %s", binaryEnv)
	}
	process := exec.CommandContext(t.Context(), binary)
	process.Env = append(process.Environ(), "SLG_GAME_ADDR=127.0.0.1:0", "SLG_GAME_RPC_ADDR="+address,
		"SLG_MYSQL_DSN=", "SLG_GAME_MYSQL_DSN=", "SLG_MYSQL_ADDRESS=", "SLG_WORLD_ID=", "SLG_WORLD_CHECKPOINT_SECONDS=30", "SLG_WORLD_RADIUS=2")
	process.Env = append(process.Env, extraEnv...)
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
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not start", binaryEnv)
	}
	return "", nil
}
