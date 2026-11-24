package testcases

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NumberMan1/orbit/rpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/NumberMan1/slg-server/database"
	"github.com/NumberMan1/slg-server/infrastructure/storage"
	"github.com/NumberMan1/slg-server/internal/gateway"
	pb "github.com/NumberMan1/slg-server/proto"
)

func recoveryDatabase(t *testing.T) (*sql.DB, string, string) {
	t.Helper()
	dsn := os.Getenv("SLG_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set SLG_TEST_MYSQL_DSN for world recovery integration")
	}
	t.Setenv("SLG_MYSQL_DSN", dsn)
	t.Setenv("SLG_MYSQL_ADDRESS", "")
	t.Setenv("SLG_WORLD_IDLE_SECONDS", "60")
	db, err := storage.OpenMySQL(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err = database.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	worldID := "recovery-" + rand.Text()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		for _, statement := range []string{"DELETE FROM slg_world_orders WHERE world_id = ?", "DELETE FROM slg_world_events WHERE world_id = ?",
			"DELETE FROM slg_world_streams WHERE world_id = ?", "DELETE FROM slg_world_snapshots WHERE world_id = ?",
			"DELETE FROM slg_world_previous_snapshots WHERE world_id = ?", "DELETE FROM slg_world_snapshot_checksums WHERE world_id = ?",
			"DELETE FROM slg_world_leases WHERE world_id = ?"} {
			if _, err := db.ExecContext(ctx, statement, worldID); err != nil {
				t.Error(err)
			}
		}
	})
	return db, dsn, worldID
}

func TestWorldRecoversCombatAndDeduplicatesCommandsWithMySQL(t *testing.T) {
	db, dsn, worldID := recoveryDatabase(t)
	separator := "?"
	if strings.Contains(dsn[strings.LastIndex(dsn, ")/")+1:], "?") {
		separator = "&"
	}
	dsn += separator + "timeout=2s&readTimeout=2s&writeTimeout=2s"
	start := func() (string, func(string, *pb.WorldRequest) *pb.WorldReply, func()) {
		t.Helper()
		address, stop := startCPPGameServer(t, "127.0.0.1:0", "SLG_GAME_MYSQL_DSN="+dsn, "SLG_WORLD_ID="+worldID,
			"SLG_WORLD_CHECKPOINT_SECONDS=300")
		return address, directWorld(t, address), stop
	}
	address, world, stop := start()
	const west, east = "west", "east"
	read := func(account string) *pb.WorldSnapshot {
		t.Helper()
		reply := world(account, nil)
		if reply.GetResult() != pb.WorldResult_WORLD_RESULT_OK || reply.GetSnapshot() == nil {
			t.Fatalf("could not read world for %s: %v", account, reply)
		}
		return reply.GetSnapshot()
	}
	send := func(account string, order *pb.WorldRequest) *pb.WorldReply {
		t.Helper()
		reply := world(account, order)
		if reply.GetResult() != pb.WorldResult_WORLD_RESULT_OK {
			t.Fatalf("could not submit %s: %v", order.GetOrder().GetCommandId(), reply)
		}
		return reply
	}
	waitFor := func(expect func(*pb.WorldSnapshot) bool) *pb.WorldSnapshot {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for {
			view := read(west)
			if expect(view) {
				return view
			}
			if time.Now().After(deadline) {
				t.Fatalf("world did not reach the expected state: %v", view)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	westHome, eastHome := read(west), read(east)
	instance := westHome.GetInstance()
	request := func(id string, target uint32, troops uint32) *pb.WorldRequest {
		return &pb.WorldRequest{KnownInstance: instance, Order: &pb.MarchOrder{CommandId: id, TargetTileId: target, Troops: troops}}
	}
	westBorder := request("west-border", tileAt(westHome, -1, 0).GetId(), 20)
	// 锁住事件流的序号行模拟慢写。局内读取须保持可用，回执和行军须等到提交。
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var savedRevision uint64
	if err := tx.QueryRowContext(t.Context(), "SELECT revision FROM slg_world_streams WHERE world_id = ? FOR UPDATE", worldID).Scan(&savedRevision); err != nil {
		t.Fatal(err)
	}
	parallel, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parallel.Close() })
	body, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(&pb.WorldCommand{Account: west, Request: westBorder})
	if err != nil {
		t.Fatal(err)
	}
	if err = rpc.Write(parallel, rpc.Frame{ID: uint16(pb.WorldMessageId_WORLD_MESSAGE_ID_REQUEST), Sequence: 1, Payload: body}, 8192); err != nil {
		t.Fatal(err)
	}
	if err = parallel.SetReadDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var timeout net.Error
	if frame, err := rpc.Read(parallel, 8192); !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("march returned before the database lock was released: %v, %v", frame, err)
	}
	// 满员访客直接读取已提交的席位结果，不会填满写请求正在使用的 RPC 池。
	visitors, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = visitors.Close() })
	if err = visitors.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for i := range 7 {
		body, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(
			&pb.WorldCommand{Account: "visitor-" + strconv.Itoa(i), Request: &pb.WorldRequest{}})
		if err != nil {
			t.Fatal(err)
		}
		if err = rpc.Write(visitors, rpc.Frame{ID: uint16(pb.WorldMessageId_WORLD_MESSAGE_ID_REQUEST), Sequence: uint32(i + 1), Payload: body}, 8192); err != nil {
			t.Fatal(err)
		}
	}
	readAt := time.Now()
	if view := read(east); len(view.GetMarches()) != 0 || time.Since(readAt) > time.Second {
		t.Fatalf("read was blocked or published an uncommitted march: %v", view)
	}
	for range 7 {
		frame, err := rpc.Read(visitors, 8192)
		var reply pb.WorldReply
		if err != nil || protojson.Unmarshal(frame.Payload, &reply) != nil || reply.GetResult() != pb.WorldResult_WORLD_RESULT_FULL {
			t.Fatalf("visitor did not receive a full-world response before commit: %v %v", &reply, err)
		}
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err = parallel.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var written pb.WorldReply
	frame, err := rpc.Read(parallel, 8192)
	if err != nil || frame.ID != uint16(pb.WorldMessageId_WORLD_MESSAGE_ID_REPLY) || frame.Sequence != 1 ||
		protojson.Unmarshal(frame.Payload, &written) != nil || written.GetResult() != pb.WorldResult_WORLD_RESULT_OK ||
		len(written.GetSnapshot().GetMarches()) != 1 {
		t.Fatalf("march was not published after the database commit: %v, %v", &written, err)
	}
	send(east, request("east-border", tileAt(eastHome, 1, 0).GetId(), 20))
	ready := waitFor(func(view *pb.WorldSnapshot) bool {
		return tileAt(view, -1, 0).GetOwner() == 1 && tileAt(view, 1, 0).GetOwner() == 2
	})
	center := tileAt(ready, 0, 0).GetId()
	send(west, request("hold-center", center, 50))
	waitFor(func(view *pb.WorldSnapshot) bool { return tileAt(view, 0, 0).GetOwner() == 1 })
	attack := request("attack-center", center, 50)
	attack.Order.ContinuousBattle = true
	send(east, attack)
	live := waitFor(func(view *pb.WorldSnapshot) bool { return len(view.GetBattles()) == 1 })
	battleID := live.GetBattles()[0].GetBattleId()
	westSupport := request("west-support", center, 25)
	eastSupport := request("east-support", center, 25)
	westSupport.Order.ReinforceBattleId, eastSupport.Order.ReinforceBattleId = battleID, battleID
	send(west, westSupport)
	send(east, eastSupport)
	stop()
	// 援军在停机期间保持在途。重启后重建剩余行军时间和战斗截止时间。
	time.Sleep(3100 * time.Millisecond)
	_, world, stop = start()
	restored := read(west)
	if restored.GetInstance() != instance || len(restored.GetMarches()) != 2 ||
		len(restored.GetBattles()) != 1 || restored.GetBattles()[0].GetBattleId() != battleID {
		t.Fatalf("world did not restore the active battle and both marches: %v", restored)
	}
	send(west, westSupport)
	send(east, eastSupport)
	changed := proto.CloneOf(westSupport)
	changed.Order.Troops = 20
	if got := world(west, changed).GetResult(); got != pb.WorldResult_WORLD_RESULT_INVALID_ORDER {
		t.Fatalf("stored command did not reject a changed retry: %v", got)
	}
	waitFor(func(view *pb.WorldSnapshot) bool { return len(view.GetMarches()) == 0 && len(view.GetBattles()) == 1 })
	retreat := request("west-retreat", center, 0)
	retreat.Order.RetreatBattleId = battleID
	send(west, retreat)
	settled := read(east)
	report := settled.GetLastBattle()
	if report.GetRetreatedPlayer() != 1 || report.GetAttackerReinforcements() != 25 || report.GetDefenderReinforcements() != 25 ||
		!report.GetCaptured() || tileAt(settled, 0, 0).GetOwner() != 2 {
		t.Fatalf("restored battle did not commit the expected result: %v", settled)
	}
	stop()
	_, world, _ = start()
	send(west, retreat)
	var cursor uint64
	reply := world(east, &pb.WorldRequest{KnownInstance: instance, KnownReportId: &cursor})
	if len(reply.GetBattleReports()) != 1 || !proto.Equal(reply.GetBattleReports()[0], report) ||
		tileAt(reply.GetSnapshot(), 0, 0).GetOwner() != 2 {
		t.Fatalf("settlement retry did not retain one durable report and ownership change: %v", reply)
	}
	var receipts, embedded int
	if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM slg_world_orders WHERE world_id = ?", worldID).Scan(&receipts); err != nil || receipts != 7 {
		t.Fatalf("expected seven successful commands after retries: %d, %v", receipts, err)
	}
	if err := db.QueryRowContext(t.Context(), `SELECT JSON_CONTAINS_PATH(CONVERT(payload USING utf8mb4), 'one', '$.players[*].orders')
		FROM slg_world_snapshots WHERE world_id = ?`, worldID).Scan(&embedded); err != nil || embedded != 0 {
		t.Fatalf("command history should not be embedded in new snapshots: %d, %v", embedded, err)
	}
	var snapshotRevision, head, entries uint64
	if err := db.QueryRowContext(t.Context(), `SELECT p.revision, s.revision, (SELECT COUNT(*) FROM slg_world_events WHERE world_id=p.world_id)
		FROM slg_world_snapshots p JOIN slg_world_streams s USING (world_id) WHERE p.world_id=?`, worldID).
		Scan(&snapshotRevision, &head, &entries); err != nil || snapshotRevision != 0 || head == 0 || entries != head {
		t.Fatalf("world mutations should advance the event stream without replacing the checkpoint: snapshot %d head %d entries %d error %v",
			snapshotRevision, head, entries, err)
	}
}

func TestWorldMigratesLegacyOrdersWithMySQL(t *testing.T) {
	db, dsn, worldID := recoveryDatabase(t)
	tiles := make([]map[string]int, 19)
	for index := range tiles {
		tiles[index] = map[string]int{"owner": 0, "garrison": 0}
	}
	tiles[7]["owner"] = 1
	legacy := map[string]any{
		"format": 1, "instance": "legacy-instance", "revision": 4, "next_revision": 5,
		"next_battle": 1, "next_report": 1, "tiles": tiles, "battles": []any{}, "reports": []any{},
		"players": []any{map[string]any{
			"account": "west", "reserves": 100, "reserve_revision": 0, "last_reinforcement_ms": 0,
			"orders": map[string]any{"old-order": map[string]int{"target": 9, "troops": 20, "mode": 0, "reinforcement": 0, "retreat": 0}},
		}, nil},
	}
	payload, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(t.Context(), "INSERT INTO slg_world_snapshots (world_id,revision,payload) VALUES (?,1,?)", worldID, payload); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		address, stop := startCPPGameServer(t, "127.0.0.1:0", "SLG_GAME_MYSQL_DSN="+dsn, "SLG_WORLD_ID="+worldID)
		world := directWorld(t, address)
		retry := &pb.WorldRequest{KnownInstance: "legacy-instance", Order: &pb.MarchOrder{CommandId: "old-order", TargetTileId: 9, Troops: 20}}
		reply := world("west", retry)
		if reply.GetResult() != pb.WorldResult_WORLD_RESULT_OK || len(reply.GetSnapshot().GetMarches()) != 0 || reply.GetSnapshot().GetAvailableTroops() != 100 {
			t.Fatalf("legacy command was lost or reapplied during upgrade: %v", reply)
		}
		stop()
	}
	var receipts, format int
	if err = db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM slg_world_orders WHERE world_id = ? AND command_id = 'old-order'", worldID).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatalf("legacy receipt was not migrated exactly once: %d, %v", receipts, err)
	}
	if err = db.QueryRowContext(t.Context(), `SELECT JSON_EXTRACT(CONVERT(payload USING utf8mb4), '$.format')
		FROM slg_world_snapshots WHERE world_id = ?`, worldID).Scan(&format); err != nil || format != 3 {
		t.Fatalf("legacy map was not upgraded: %d, %v", format, err)
	}
}

func TestWorldCheckpointPreservesSubMillisecondArrivalOrder(t *testing.T) {
	db, dsn, worldID := recoveryDatabase(t)
	const strike = int64(3 * time.Second)
	const lateBy = int64(200 * time.Microsecond)
	tiles := make([]map[string]int, 19)
	for index := range tiles {
		tiles[index] = map[string]int{"owner": 0, "garrison": 0}
	}
	tiles[7]["owner"], tiles[11]["owner"] = 1, 2
	tiles[9]["owner"], tiles[9]["garrison"] = 2, 1
	checkpoint := map[string]any{
		"format": 2, "instance": "precision-instance", "revision": 4, "next_revision": 5,
		"next_battle": 2, "next_report": 1, "tiles": tiles, "reports": []any{},
		"players": []any{
			map[string]any{"account": "west", "seat": 2, "reserves": 100, "reserve_revision": 0, "last_reinforcement_ns": 0,
				"march": map[string]any{"source": 8, "target": 10, "troops": 10, "arrives_ns": strike + lateBy,
					"sequence": 4, "mode": 0, "reinforcement": 1}},
			map[string]any{"account": "east", "seat": 3, "reserves": 100, "reserve_revision": 0, "last_reinforcement_ns": 0},
		},
		"battles": []any{map[string]any{
			"id": 1, "target": 10, "attacker": 1, "defender": 2, "rule": 4, "attacker_troops": 1, "defender_troops": 1,
			"round_seed": 123, "seed": 123, "round": 1, "rounds": 0, "round_ends_ns": strike, "initial_attacker": 1,
			"initial_defender": 1, "attacker_waiting": 0, "defender_waiting": 0, "attacker_reinforcements": 0, "defender_reinforcements": 0,
		}},
	}
	payload, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(t.Context(), "INSERT INTO slg_world_snapshots (world_id,revision,payload) VALUES (?,1,?)", worldID, payload); err != nil {
		t.Fatal(err)
	}
	start := func() (func(string, *pb.WorldRequest) *pb.WorldReply, func()) {
		address, stop := startCPPGameServer(t, "127.0.0.1:0", "SLG_GAME_MYSQL_DSN="+dsn, "SLG_WORLD_ID="+worldID)
		return directWorld(t, address), stop
	}
	world, stop := start()
	// 守方新的行军只生成一条事件。晚到的攻方援军和决胜交锋仍从检查点恢复。
	east := world("east", nil).GetSnapshot()
	order := knownWorld(east)
	order.Order = &pb.MarchOrder{CommandId: "east-border", TargetTileId: tileAt(east, 1, 0).GetId(), Troops: 20}
	if reply := world("east", order); reply.GetResult() != pb.WorldResult_WORLD_RESULT_OK {
		t.Fatalf("could not trigger checkpoint write: %v", reply)
	}
	var savedDifference int64
	err = db.QueryRowContext(t.Context(), `SELECT
		CAST(JSON_EXTRACT(CONVERT(payload USING utf8mb4), '$.players[0].march.arrives_ns') AS SIGNED) -
		CAST(JSON_EXTRACT(CONVERT(payload USING utf8mb4), '$.battles[0].round_ends_ns') AS SIGNED)
		FROM slg_world_snapshots WHERE world_id = ?`, worldID).Scan(&savedDifference)
	if err != nil || savedDifference != lateBy {
		t.Fatalf("checkpoint changed the separation between arrival and strike: %d, %v", savedDifference, err)
	}
	var battleWritten, firstChangedPlayer int
	err = db.QueryRowContext(t.Context(), `SELECT JSON_CONTAINS_PATH(CONVERT(payload USING utf8mb4), 'one', '$.battles'),
		JSON_EXTRACT(CONVERT(payload USING utf8mb4), '$.players[0][0]')
		FROM slg_world_events WHERE world_id = ? ORDER BY revision DESC LIMIT 1`, worldID).Scan(&battleWritten, &firstChangedPlayer)
	if err != nil || battleWritten != 0 || firstChangedPlayer != 1 {
		t.Fatalf("march event included unrelated battle state or the wrong player: %d, %d, %v", battleWritten, firstChangedPlayer, err)
	}
	stop()
	world, _ = start()
	deadline := time.Now().Add(7 * time.Second)
	for {
		view := world("east", nil).GetSnapshot()
		if report := view.GetLastBattle(); report != nil {
			if report.GetCaptured() || report.GetAttackerReinforcements() != 0 || tileAt(view, 0, 0).GetOwner() != 2 {
				t.Fatalf("a late reinforcement changed the result after restore: %v", view)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("restored battle did not finish: %v", view)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestWorldCompactsCheckpointAndReplaysTailWithMySQL(t *testing.T) {
	db, dsn, worldID := recoveryDatabase(t)
	start := func() (func(string, *pb.WorldRequest) *pb.WorldReply, func()) {
		t.Helper()
		address, stop := startCPPGameServer(t, "127.0.0.1:0", "SLG_GAME_MYSQL_DSN="+dsn, "SLG_WORLD_ID="+worldID,
			"SLG_WORLD_CHECKPOINT_SECONDS=1")
		return directWorld(t, address), stop
	}
	world, stop := start()
	west := world("west", nil).GetSnapshot()
	var checkpoint, head, entries uint64
	waitCheckpoint := func(revision, count uint64) {
		t.Helper()
		deadline := time.Now().Add(6 * time.Second)
		for {
			err := db.QueryRowContext(t.Context(), `SELECT p.revision, s.revision,
			(SELECT COUNT(*) FROM slg_world_events WHERE world_id=p.world_id)
			FROM slg_world_snapshots p JOIN slg_world_streams s USING (world_id) WHERE p.world_id=?`, worldID).
				Scan(&checkpoint, &head, &entries)
			if err != nil {
				t.Fatal(err)
			}
			if checkpoint == revision && head == revision && entries == count {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("checkpoint did not preserve the fallback tail: snapshot %d head %d entries %d", checkpoint, head, entries)
			}
			time.Sleep(40 * time.Millisecond)
		}
	}
	waitCheckpoint(1, 1)
	east := world("east", nil).GetSnapshot()
	if west.GetYourPlayer() != 1 || east.GetYourPlayer() != 2 {
		t.Fatalf("players did not join the journaled world: %v / %v", west, east)
	}
	waitCheckpoint(2, 1)

	// 阻塞下一次检查点更新。两个新命令仍须仅等待日志提交，且都能从检查点之后重放。
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(t.Context(), "SELECT revision FROM slg_world_snapshots WHERE world_id=? FOR UPDATE", worldID).Scan(&checkpoint); err != nil {
		t.Fatal(err)
	}
	order := func(s *pb.WorldSnapshot, q int32, id string) *pb.WorldRequest {
		request := knownWorld(s)
		request.Order = &pb.MarchOrder{CommandId: id, TargetTileId: tileAt(s, q, 0).GetId(), Troops: 20}
		return request
	}
	westOrder, eastOrder := order(west, -1, "tail-west"), order(east, 1, "tail-east")
	if reply := world("west", westOrder); reply.GetResult() != pb.WorldResult_WORLD_RESULT_OK {
		t.Fatalf("event append waited on locked checkpoint: %v", reply)
	}
	time.Sleep(1100 * time.Millisecond)
	startAt := time.Now()
	if reply := world("east", eastOrder); reply.GetResult() != pb.WorldResult_WORLD_RESULT_OK || time.Since(startAt) > time.Second {
		t.Fatalf("slow background checkpoint blocked acknowledgement: %v", reply)
	}
	stop()
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	// 最新快照损坏时，上一份快照和从该快照开始的日志仍足以恢复。
	if _, err := db.ExecContext(t.Context(), "UPDATE slg_world_snapshots SET payload='not-json' WHERE world_id=?", worldID); err != nil {
		t.Fatal(err)
	}
	world, _ = start()
	retry := world("west", westOrder)
	if retry.GetResult() != pb.WorldResult_WORLD_RESULT_OK || retry.GetSnapshot().GetInstance() != west.GetInstance() ||
		len(retry.GetSnapshot().GetMarches()) != 2 {
		t.Fatalf("the two journal entries after the checkpoint did not restore once: %v", retry)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		var repaired uint64
		var valid bool
		if err := db.QueryRowContext(t.Context(), "SELECT revision, JSON_VALID(CONVERT(payload USING utf8mb4)) FROM slg_world_snapshots WHERE world_id=?", worldID).Scan(&repaired, &valid); err != nil {
			t.Fatal(err)
		}
		if valid {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("latest checkpoint was not repaired after fallback: revision %d", repaired)
		}
		time.Sleep(40 * time.Millisecond)
	}
}

func TestWorldResumesCoveredEventCleanupAfterRestartWithMySQL(t *testing.T) {
	db, dsn, worldID := recoveryDatabase(t)
	start := func() func() {
		t.Helper()
		_, stop := startCPPGameServer(t, "127.0.0.1:0", "SLG_GAME_MYSQL_DSN="+dsn, "SLG_WORLD_ID="+worldID,
			"SLG_WORLD_CHECKPOINT_SECONDS=300")
		return stop
	}
	stop := start()
	stop()

	// 模拟快照已提交、后台还未来得及清理就退出。覆盖量超过两个清理批次。
	const covered = 513
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, statement := range []string{"UPDATE slg_world_snapshots SET revision=? WHERE world_id=?",
		"UPDATE slg_world_streams SET revision=? WHERE world_id=?"} {
		if _, err := tx.ExecContext(t.Context(), statement, covered+1, worldID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.ExecContext(t.Context(), `INSERT INTO slg_world_previous_snapshots (world_id,revision,payload)
		SELECT world_id,?,payload FROM slg_world_snapshots WHERE world_id=?`, covered, worldID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), `INSERT INTO slg_world_snapshot_checksums (world_id,revision,digest)
		SELECT world_id,revision,UNHEX(SHA2(payload,256)) FROM slg_world_previous_snapshots WHERE world_id=?`, worldID); err != nil {
		t.Fatal(err)
	}
	rows := strings.TrimSuffix(strings.Repeat("(?,?,'already covered'),", covered), ",")
	parameters := make([]any, 0, covered*2)
	for index := 1; index <= covered; index++ {
		parameters = append(parameters, worldID, index)
	}
	if _, err := tx.ExecContext(t.Context(), "INSERT INTO slg_world_events (world_id,revision,payload) VALUES "+rows, parameters...); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	start()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var checkpoint, head, entries uint64
		if err := db.QueryRowContext(t.Context(), `SELECT p.revision, s.revision,
			(SELECT COUNT(*) FROM slg_world_events WHERE world_id=p.world_id)
			FROM slg_world_snapshots p JOIN slg_world_streams s USING (world_id) WHERE p.world_id=?`, worldID).
			Scan(&checkpoint, &head, &entries); err != nil {
			t.Fatal(err)
		}
		if checkpoint != covered+1 || head != covered+1 {
			t.Fatalf("cleanup should not advance the snapshot or event stream: %d / %d", checkpoint, head)
		}
		if entries == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cleanup did not resume after restart without new events: %d events remain", entries)
		}
		time.Sleep(40 * time.Millisecond)
	}
}

func TestWorldSavesNewCheckpointWhileEventCleanupIsBlockedWithMySQL(t *testing.T) {
	db, dsn, worldID := recoveryDatabase(t)
	separator := "?"
	if strings.Contains(dsn[strings.LastIndex(dsn, ")/")+1:], "?") {
		separator = "&"
	}
	address, _ := startCPPGameServer(t, "127.0.0.1:0", "SLG_GAME_MYSQL_DSN="+dsn+separator+"readTimeout=8s",
		"SLG_WORLD_ID="+worldID, "SLG_WORLD_CHECKPOINT_SECONDS=1")
	world := directWorld(t, address)
	west := world("west", nil).GetSnapshot()
	waitCheckpoint := func(expected uint64) {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		for {
			var revision uint64
			if err := db.QueryRowContext(t.Context(), "SELECT revision FROM slg_world_snapshots WHERE world_id=?", worldID).Scan(&revision); err != nil {
				t.Fatal(err)
			}
			if revision >= expected {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("snapshot did not advance to %d while cleanup was locked", expected)
			}
			time.Sleep(40 * time.Millisecond)
		}
	}
	waitCheckpoint(1)
	hold, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback()
	var covered uint64
	if err := hold.QueryRowContext(t.Context(), "SELECT revision FROM slg_world_events WHERE world_id=? AND revision=1 FOR UPDATE", worldID).Scan(&covered); err != nil {
		t.Fatal(err)
	}
	if east := world("east", nil).GetSnapshot(); east.GetYourPlayer() != 2 {
		t.Fatalf("blocked event cleanup prevented a new seat: %v", east)
	}
	waitCheckpoint(2)
	order := knownWorld(west)
	order.Order = &pb.MarchOrder{CommandId: "next-snapshot-while-cleaning", TargetTileId: tileAt(west, -1, 0).GetId(), Troops: 20}
	if reply := world("west", order); reply.GetResult() != pb.WorldResult_WORLD_RESULT_OK {
		t.Fatalf("blocked event cleanup prevented a new append: %v", reply)
	}
	waitCheckpoint(3)
}

func TestWorldKeepsVerifiedFallbackWhenCurrentSnapshotIsCorruptWithMySQL(t *testing.T) {
	db, dsn, worldID := recoveryDatabase(t)
	start := func() (func(string, *pb.WorldRequest) *pb.WorldReply, func()) {
		t.Helper()
		address, stop := startCPPGameServer(t, "127.0.0.1:0", "SLG_GAME_MYSQL_DSN="+dsn, "SLG_WORLD_ID="+worldID,
			"SLG_WORLD_CHECKPOINT_SECONDS=1")
		return directWorld(t, address), stop
	}
	world, stop := start()
	west := world("west", nil).GetSnapshot()
	check := func(expected uint64) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			var latest, fallback uint64
			var retained int
			if err := db.QueryRowContext(t.Context(), `SELECT s.revision,COALESCE(p.revision,0),
				(SELECT COUNT(*) FROM slg_world_events WHERE world_id=s.world_id AND revision=1)
				FROM slg_world_snapshots s LEFT JOIN slg_world_previous_snapshots p USING (world_id) WHERE s.world_id=?`, worldID).
				Scan(&latest, &fallback, &retained); err != nil {
				t.Fatal(err)
			}
			if latest == expected {
				if fallback != 0 || retained != 1 {
					t.Fatalf("checkpoint %d discarded the valid fallback or its tail: fallback %d, event count %d", latest, fallback, retained)
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("checkpoint did not reach %d: last %d", expected, latest)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	check(1)
	// 篡改为仍可解析的 JSON，验证轮换按原始内容校验而非语法校验。
	if _, err := db.ExecContext(t.Context(), `UPDATE slg_world_snapshots SET payload=JSON_SET(CAST(payload AS JSON),
		'$.instance','corrupt-current') WHERE world_id=?`, worldID); err != nil {
		t.Fatal(err)
	}
	if east := world("east", nil).GetSnapshot(); east.GetYourPlayer() != 2 {
		t.Fatalf("east did not join: %v", east)
	}
	check(2)
	stop()
	if _, err := db.ExecContext(t.Context(), "UPDATE slg_world_snapshot_checksums SET digest=UNHEX(REPEAT('00',32)) WHERE world_id=? AND revision=2",
		worldID); err != nil {
		t.Fatal(err)
	}
	world, _ = start()
	if got := world("west", nil).GetSnapshot(); got.GetYourPlayer() != 1 || got.GetInstance() != west.GetInstance() {
		t.Fatalf("fallback could not recover the first join: %v", got)
	}
	if got := world("east", nil).GetSnapshot(); got.GetYourPlayer() != 2 {
		t.Fatalf("fallback could not recover the second join: %v", got)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var valid bool
		if err := db.QueryRowContext(t.Context(), `SELECT c.digest=UNHEX(SHA2(s.payload,256)) FROM slg_world_snapshots s
			JOIN slg_world_snapshot_checksums c USING (world_id,revision) WHERE s.world_id=?`, worldID).Scan(&valid); err != nil {
			t.Fatal(err)
		}
		if valid {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("recovery did not repair a bad checksum at the existing revision")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestWorldExpandsMapAndReplaysNewTilesWithMySQL(t *testing.T) {
	_, dsn, worldID := recoveryDatabase(t)
	start := func(radius int) (func(string, *pb.WorldRequest) *pb.WorldReply, func()) {
		t.Helper()
		address, stop := startCPPGameServer(t, "127.0.0.1:0", "SLG_GAME_MYSQL_DSN="+dsn, "SLG_WORLD_ID="+worldID,
			"SLG_WORLD_CHECKPOINT_SECONDS=300", "SLG_WORLD_RADIUS="+strconv.Itoa(radius))
		return directWorld(t, address), stop
	}
	world, stop := start(2)
	old := world("west", nil).GetSnapshot()
	stop()
	world, stop = start(7)
	expanded := world("west", knownWorld(old)).GetSnapshot()
	if len(old.GetTiles()) != 19 || len(expanded.GetTiles()) != 169 || expanded.GetInstance() != old.GetInstance() {
		t.Fatalf("the new layout should refresh the client while preserving the world instance: %v", expanded)
	}
	for _, tile := range old.GetTiles() {
		if got := tileAt(expanded, tile.GetQ(), tile.GetR()); !proto.Equal(got, tile) {
			t.Fatalf("existing tile changed on expansion: %v / %v", tile, got)
		}
	}
	order := knownWorld(expanded)
	order.Order = &pb.MarchOrder{CommandId: "first-outside-tile", TargetTileId: tileAt(expanded, -3, 0).GetId(), Troops: 20}
	if reply := world("west", order); reply.GetResult() != pb.WorldResult_WORLD_RESULT_OK || len(reply.GetSnapshot().GetMarches()) != 1 {
		t.Fatalf("new tile could not receive a march: %v", reply)
	}
	deadline := time.Now().Add(7 * time.Second)
	for tileAt(world("west", nil).GetSnapshot(), -3, 0).GetOwner() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("march did not capture a new tile")
		}
		time.Sleep(60 * time.Millisecond)
	}
	stop()
	world, _ = start(7)
	if view := world("west", nil).GetSnapshot(); len(view.GetTiles()) != 169 || tileAt(view, -3, 0).GetOwner() != 1 {
		t.Fatalf("replay from the old checkpoint lost the newly allocated tile: %v", view)
	}
}

func TestWorldRetiresSeatWhileOrderCleanupIsBlockedWithMySQL(t *testing.T) {
	db, dsn, worldID := recoveryDatabase(t)
	address, _ := startCPPGameServer(t, "127.0.0.1:0", "SLG_GAME_MYSQL_DSN="+dsn, "SLG_WORLD_ID="+worldID,
		"SLG_WORLD_IDLE_SECONDS=2")
	world := directWorld(t, address)
	west := world("west", nil).GetSnapshot()
	if east := world("east", nil).GetSnapshot(); west.GetYourPlayer() != 1 || east.GetYourPlayer() != 2 {
		t.Fatalf("both players must occupy a seat: %v / %v", west, east)
	}
	dispatch := knownWorld(west)
	dispatch.Order = &pb.MarchOrder{CommandId: "retiring-seat", TargetTileId: tileAt(west, -1, 0).GetId(), Troops: 20}
	if reply := world("west", dispatch); reply.GetResult() != pb.WorldResult_WORLD_RESULT_OK {
		t.Fatalf("old seat did not commit a receipt: %v", reply)
	}
	hold, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback()
	var seat uint64
	if err := hold.QueryRowContext(t.Context(), "SELECT seat_revision FROM slg_world_orders WHERE world_id=? AND command_id=? FOR UPDATE",
		worldID, dispatch.Order.CommandId).Scan(&seat); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		world("east", nil)
		if view := world("replacement", nil).GetSnapshot(); view.GetYourPlayer() == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("seat recycling and reuse waited for receipt cleanup")
		}
		time.Sleep(60 * time.Millisecond)
	}
	if err := hold.Rollback(); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		var entries int
		if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM slg_world_orders WHERE world_id=? AND seat_revision=?", worldID, seat).Scan(&entries); err != nil {
			t.Fatal(err)
		}
		if entries == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("retired receipt was not cleaned")
		}
		time.Sleep(60 * time.Millisecond)
	}
}

func TestWorldLeasePreventsTwoServingInstancesWithMySQL(t *testing.T) {
	db, dsn, worldID := recoveryDatabase(t)
	args := []string{"SLG_GAME_MYSQL_DSN=" + dsn, "SLG_WORLD_ID=" + worldID}
	address, stop := startCPPGameServer(t, "127.0.0.1:0", args...)
	previous := directWorld(t, address)
	west := previous("west", nil).GetSnapshot()
	if west.GetYourPlayer() != 1 {
		t.Fatalf("west did not get a seat: %v", west)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	duplicate := exec.CommandContext(ctx, os.Getenv("SLG_TEST_GAME_SERVER"))
	duplicate.Env = append(duplicate.Environ(), "SLG_GAME_ADDR=127.0.0.1:0", "SLG_GAME_RPC_ADDR=127.0.0.1:0", "SLG_MYSQL_ADDRESS=")
	duplicate.Env = append(duplicate.Env, args...)
	output, err := duplicate.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "world already has an active writer") {
		t.Fatalf("a second process started while the existing lease was valid: %v, %s", err, output)
	}
	if reply := previous("west", nil); reply.GetResult() != pb.WorldResult_WORLD_RESULT_OK {
		t.Fatalf("the original world lost its lease during duplicate startup: %v", reply)
	}
	// 锁住租约，模拟续约不可用。旧实例应主动退出，允许监督器恢复进程。
	hold, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback()
	var writer string
	if err := hold.QueryRowContext(t.Context(), "SELECT writer FROM slg_world_leases WHERE world_id=? FOR UPDATE", worldID).Scan(&writer); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", address, 200*time.Millisecond)
		if err != nil {
			break
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("the expired writer did not stop listening")
		}
		time.Sleep(75 * time.Millisecond)
	}
	stop()
	if err := hold.Rollback(); err != nil {
		t.Fatal(err)
	}
	address, _ = startCPPGameServer(t, "127.0.0.1:0", args...)
	current := directWorld(t, address)
	dispatch := knownWorld(west)
	dispatch.Order = &pb.MarchOrder{CommandId: "current-writer", TargetTileId: tileAt(west, -1, 0).GetId(), Troops: 20}
	if reply := current("west", dispatch); reply.GetResult() != pb.WorldResult_WORLD_RESULT_OK || len(reply.GetSnapshot().GetMarches()) != 1 {
		t.Fatalf("current writer did not append the march: %v", reply)
	}
	if reply := current("east", nil); reply.GetResult() != pb.WorldResult_WORLD_RESULT_OK || reply.GetSnapshot().GetYourPlayer() != 2 {
		t.Fatalf("the current writer could not assign the free seat: %v", reply)
	}
}

func TestWorldKeepsLeaseDuringSlowEventWriteWithMySQL(t *testing.T) {
	db, dsn, worldID := recoveryDatabase(t)
	separator := "?"
	if strings.Contains(dsn[strings.LastIndex(dsn, ")/")+1:], "?") {
		separator = "&"
	}
	address, _ := startCPPGameServer(t, "127.0.0.1:0", "SLG_GAME_MYSQL_DSN="+dsn+separator+"readTimeout=8s", "SLG_WORLD_ID="+worldID)
	world := directWorld(t, address)
	west := world("west", nil).GetSnapshot()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var head uint64
	if err := tx.QueryRowContext(t.Context(), "SELECT revision FROM slg_world_streams WHERE world_id=? FOR UPDATE", worldID).Scan(&head); err != nil {
		t.Fatal(err)
	}
	blocked := time.AfterFunc(4200*time.Millisecond, func() { _ = tx.Rollback() })
	defer blocked.Stop()
	order := knownWorld(west)
	order.Order = &pb.MarchOrder{CommandId: "slow-head-with-lease", TargetTileId: tileAt(west, -1, 0).GetId(), Troops: 20}
	if reply := world("west", order); reply.GetResult() != pb.WorldResult_WORLD_RESULT_UNAVAILABLE {
		t.Fatalf("blocked write should hit the reply deadline: %v", reply)
	}
	if view := world("west", nil).GetSnapshot(); view.GetYourPlayer() != 1 || len(view.GetMarches()) != 0 {
		t.Fatalf("the existing process could not read its committed world while the write waited: %v", view)
	}
	reply := world("west", order)
	view := reply.GetSnapshot()
	if reply.GetResult() != pb.WorldResult_WORLD_RESULT_OK ||
		(len(view.GetMarches()) != 1 && tileAt(view, -1, 0).GetOwner() != 1) {
		t.Fatalf("the existing process did not resume when the write lock was released: %v", reply)
	}
	var receipts int
	if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM slg_world_orders WHERE world_id=? AND command_id=?", worldID, order.Order.CommandId).
		Scan(&receipts); err != nil || receipts != 1 {
		t.Fatalf("the retried order should commit once: %d, %v", receipts, err)
	}
}

func TestWorldConfirmsPriorOrderAfterAnotherSeatExpiresWithMySQL(t *testing.T) {
	_, dsn, worldID := recoveryDatabase(t)
	start := func() (func(string, *pb.WorldRequest) *pb.WorldReply, func()) {
		t.Helper()
		address, stop := startCPPGameServer(t, "127.0.0.1:0", "SLG_GAME_MYSQL_DSN="+dsn, "SLG_WORLD_ID="+worldID,
			"SLG_WORLD_IDLE_SECONDS=2")
		return directWorld(t, address), stop
	}
	world, stop := start()
	west := world("west", nil).GetSnapshot()
	if east := world("east", nil).GetSnapshot(); west.GetYourPlayer() != 1 || east.GetYourPlayer() != 2 {
		t.Fatalf("both players must occupy a seat: %v / %v", west, east)
	}
	dispatch := knownWorld(west)
	dispatch.Order = &pb.MarchOrder{CommandId: "before-east-expired", TargetTileId: tileAt(west, -1, 0).GetId(), Troops: 20}
	if reply := world("west", dispatch); reply.GetResult() != pb.WorldResult_WORLD_RESULT_OK || len(reply.GetSnapshot().GetMarches()) != 1 {
		t.Fatalf("west march was not committed: %v", reply)
	}
	deadline := time.Now().Add(6 * time.Second)
	for {
		view := world("west", nil).GetSnapshot()
		if view.GetInstance() != west.GetInstance() && view.GetYourPlayer() == 1 && tileAt(view, -1, 0).GetOwner() == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("east should expire while west finishes the march: %v", view)
		}
		time.Sleep(75 * time.Millisecond)
	}
	stop()
	world, _ = start()
	reply := world("west", dispatch)
	view := reply.GetSnapshot()
	if reply.GetResult() != pb.WorldResult_WORLD_RESULT_OK || view.GetYourPlayer() != 1 ||
		view.GetInstance() == west.GetInstance() || tileAt(view, -1, 0).GetOwner() != 1 || len(view.GetMarches()) != 0 {
		t.Fatalf("old instance should return the recovered receipt and current map without dispatching again: %v", reply)
	}
	mutated := proto.CloneOf(dispatch)
	mutated.Order.TargetTileId = tileAt(west, -2, 1).GetId()
	if reply := world("west", mutated); reply.GetResult() != pb.WorldResult_WORLD_RESULT_WORLD_CHANGED {
		t.Fatalf("a prior instance must not confirm a different payload: %v", reply)
	}
	mutated.Order.CommandId = "new-order-from-old-instance"
	if reply := world("west", mutated); reply.GetResult() != pb.WorldResult_WORLD_RESULT_WORLD_CHANGED {
		t.Fatalf("a prior instance must not dispatch a new order: %v", reply)
	}
}

func TestWorldRefreshesCachedMarchDeadlineAfterRestartWithMySQL(t *testing.T) {
	_, dsn, worldID := recoveryDatabase(t)
	gameEnv := []string{"SLG_GAME_MYSQL_DSN=" + dsn, "SLG_WORLD_ID=" + worldID}
	game, stop := startCPPGameServer(t, "127.0.0.1:0", gameEnv...)
	upstream, waitForUpstream := startWorldUpstream(t, game)
	tickets := &memoryTickets{}
	options, err := gateway.SessionOptions()
	if err != nil {
		t.Fatal(err)
	}
	tcp := startWorldTCP(t, options, gateway.NewSessions(tickets).WorldHandler(upstream))
	conn, err := net.DialTimeout("tcp", tcp.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}
	client := exchangeWorldTCP(conn, options.MaxPayload)
	authenticateWorld(t, tickets, client, "west")
	world := func(request *pb.WorldRequest) *pb.WorldReply {
		t.Helper()
		return queryWorld(t, client, request)
	}
	first := world(&pb.WorldRequest{KnownTileRevision: proto.Uint64(0), KnownReportId: proto.Uint64(0)}).GetSnapshot()
	order := knownWorld(first)
	order.KnownTileRevision = first.TileRevision
	order.KnownReportId = proto.Uint64(0)
	order.Order = &pb.MarchOrder{CommandId: "restart-clock-sync", TargetTileId: tileAt(first, -1, 0).GetId(), Troops: 20}
	cached := world(order).GetSnapshot()
	if cached.GetTileRevision() == 0 || len(cached.GetMarches()) != 1 || len(cached.GetTiles()) != 0 {
		t.Fatalf("the march should update timing while keeping the current tile cache: %v", cached)
	}
	legacy := world(&pb.WorldRequest{}).GetSnapshot()
	stop()
	sync := knownWorld(cached)
	sync.KnownTileRevision = cached.TileRevision
	sync.KnownReportId = proto.Uint64(0)
	if unavailable := world(sync); unavailable.GetResult() != pb.WorldResult_WORLD_RESULT_UNAVAILABLE {
		t.Fatalf("the gateway should keep the session open while the game server is stopped: %v", unavailable)
	}
	// 旧的绝对到达时间先在停机期间经过，再以客户端手上的版本查询恢复后的世界。
	originalArrival := cached.GetMarches()[0].GetArrivesAtMs()
	if wait := time.Until(time.UnixMilli(originalArrival).Add(150 * time.Millisecond)); wait > 0 {
		time.Sleep(wait)
	}
	startCPPGameServer(t, game, gameEnv...)
	waitForUpstream()
	updated := world(sync)
	view := updated.GetSnapshot()
	if updated.GetResult() != pb.WorldResult_WORLD_RESULT_OK || view.GetInstance() != cached.GetInstance() ||
		view.GetRevision() == cached.GetRevision() || view.GetTileRevision() != cached.GetTileRevision() ||
		len(view.GetTiles()) != 0 || len(view.GetMarches()) != 1 {
		t.Fatalf("restarting should refresh timing with the same world and tile cache: %v", updated)
	}
	if arrives := view.GetMarches()[0].GetArrivesAtMs(); arrives <= updated.GetServerTimeMs() || arrives <= originalArrival {
		t.Fatalf("the refreshed countdown must use the restored future deadline: old %d, current %d, server %d",
			originalArrival, arrives, updated.GetServerTimeMs())
	}
	if view := world(knownWorld(legacy)).GetSnapshot(); len(view.GetTiles()) != 19 || len(view.GetMarches()) != 1 {
		t.Fatalf("legacy clients should refresh timing in a full map after restart: %v", view)
	}
	if replayed := world(order); replayed.GetResult() != pb.WorldResult_WORLD_RESULT_OK || len(replayed.GetSnapshot().GetMarches()) != 1 {
		t.Fatalf("the new synchronization version must preserve command deduplication: %v", replayed)
	}
}

func TestWorldReplenishesBetweenJournalWritesWithMySQL(t *testing.T) {
	db, dsn, worldID := recoveryDatabase(t)
	start := func() (func(string, *pb.WorldRequest) *pb.WorldReply, func()) {
		t.Helper()
		address, stop := startCPPGameServer(t, "127.0.0.1:0", "SLG_GAME_MYSQL_DSN="+dsn, "SLG_WORLD_ID="+worldID,
			"SLG_WORLD_CHECKPOINT_SECONDS=300")
		return directWorld(t, address), stop
	}
	head := func() uint64 {
		t.Helper()
		var revision uint64
		if err := db.QueryRowContext(t.Context(), "SELECT revision FROM slg_world_streams WHERE world_id=?", worldID).Scan(&revision); err != nil {
			t.Fatal(err)
		}
		return revision
	}
	world, stop := start()
	west := world("west", nil).GetSnapshot()
	order := knownWorld(west)
	order.Order = &pb.MarchOrder{CommandId: "replenish-between-events", TargetTileId: tileAt(west, -1, 0).GetId(), Troops: 20}
	dispatch := world("west", order).GetSnapshot()
	if dispatch.GetAvailableTroops() != 80 || head() != 2 {
		t.Fatalf("expected only the join and dispatch events: %v", dispatch)
	}

	// 旧客户端通过快照看到补兵。新客户端只接收私有兵力，期间事件序号保持不变。
	deadline := time.Now().Add(2500 * time.Millisecond)
	for {
		reply := world("west", knownWorld(dispatch))
		if reply.GetSnapshot().GetAvailableTroops() >= 85 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("legacy client did not receive replenishment: %v", reply)
		}
		time.Sleep(40 * time.Millisecond)
	}
	current := world("west", &pb.WorldRequest{KnownReportId: proto.Uint64(0)}).GetSnapshot()
	sync := knownWorld(current)
	sync.KnownReportId = proto.Uint64(0)
	if reply := world("west", sync); reply.GetSnapshot() != nil || reply.GetAvailableTroops() < 85 || head() != 2 {
		t.Fatalf("replenishment should update reserves without a new map version or row: %v", reply)
	}

	deadline = time.Now().Add(10 * time.Second)
	for head() < 4 {
		if time.Now().After(deadline) {
			t.Fatal("arrival and the next world clock event were not saved")
		}
		time.Sleep(60 * time.Millisecond)
	}
	var containsObjects bool
	if err := db.QueryRowContext(t.Context(), `SELECT JSON_CONTAINS_PATH(CAST(payload AS JSON), 'one', '$.players', '$.tiles', '$.battles')
		FROM slg_world_events WHERE world_id=? AND revision=4`, worldID).Scan(&containsObjects); err != nil || containsObjects {
		t.Fatalf("the natural replenishment should only advance the persisted clock: objects=%v, error=%v", containsObjects, err)
	}
	stop()
	world, _ = start()
	view := world("west", nil).GetSnapshot()
	if view.GetAvailableTroops() != 100 || tileAt(view, -1, 0).GetOwner() != 1 || head() != 4 {
		t.Fatalf("restoring should recalculate the full reserve from the saved clock: %v", view)
	}
}
