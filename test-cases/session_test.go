package testcases

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	serverlog "github.com/NumberMan1/log"
	"github.com/NumberMan1/orbit/rpc"
	orbittcp "github.com/NumberMan1/orbit/socket/tcp"
	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"

	"github.com/NumberMan1/slg-server/database"
	"github.com/NumberMan1/slg-server/infrastructure/storage"
	"github.com/NumberMan1/slg-server/internal/gateway"
	"github.com/NumberMan1/slg-server/internal/loginserver"
	"github.com/NumberMan1/slg-server/internal/sessionticket"
	sessionpb "github.com/NumberMan1/slg-server/proto"
	"github.com/NumberMan1/slg-server/testkit"
)

func TestLoginAndTCPConnect(t *testing.T) {
	store := &memoryTickets{}
	login := loginserver.NewHandler(testAccount{}, store, serverlog.DefaultLogger())
	runLoginAndTCPConnect(t, "demo", login, gateway.NewSessions(store))
}

func TestLoginWithMySQLAndRedis(t *testing.T) {
	dsn, addresses := os.Getenv("SLG_TEST_MYSQL_DSN"), os.Getenv("SLG_TEST_REDIS_ADDRESSES")
	if dsn == "" || addresses == "" {
		t.Skip("set SLG_TEST_MYSQL_DSN and SLG_TEST_REDIS_ADDRESSES for middleware integration")
	}
	t.Setenv("SLG_MYSQL_DSN", dsn)
	t.Setenv("SLG_REDIS_ADDRESSES", addresses)
	mode := os.Getenv("SLG_TEST_REDIS_MODE")
	if mode == "" {
		mode = "standalone"
	}
	t.Setenv("SLG_REDIS_MODE", mode)
	t.Setenv("SLG_REDIS_PASSWORD", os.Getenv("SLG_TEST_REDIS_PASSWORD"))
	db, err := storage.OpenMySQL(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := database.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	accounts, err := loginserver.NewAccounts(db)
	if err != nil {
		t.Fatal(err)
	}
	account := "test-" + rand.Text()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := db.ExecContext(ctx, "DELETE FROM slg_accounts WHERE account = ?", account); err != nil {
			t.Error(err)
		}
	})
	for range 2 {
		if err := accounts.Create(t.Context(), account, "test-password"); err != nil {
			t.Fatal(err)
		}
	}
	if err := accounts.Create(t.Context(), account, "different-password"); err == nil {
		t.Fatal("account creation replaced the existing password")
	}
	issueClient, err := storage.OpenRedis(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = issueClient.Close() })
	consumeClient, err := storage.OpenRedis(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = consumeClient.Close() })
	prefix := "slg:test:" + rand.Text()
	login := loginserver.NewHandler(accounts, sessionticket.NewRedisTickets(issueClient, prefix), serverlog.DefaultLogger())
	consumer := gateway.NewSessions(sessionticket.NewRedisTickets(consumeClient, prefix))
	runLoginAndTCPConnect(t, account, login, consumer)
}

type testAccount struct{}

func (testAccount) Authenticate(_ context.Context, account, password string) error {
	if account != "demo" || password != "test-password" {
		return loginserver.ErrCredentials
	}
	return nil
}

type memoryTickets struct {
	mu      sync.Mutex
	tickets map[string]string
}

func (s *memoryTickets) Issue(_ context.Context, account string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tickets == nil {
		s.tickets = make(map[string]string)
	}
	ticket := rand.Text()
	s.tickets[ticket] = account
	return ticket, nil
}

func (s *memoryTickets) Consume(_ context.Context, ticket string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	account, exists := s.tickets[ticket]
	if !exists {
		return "", errors.New("ticket missing")
	}
	delete(s.tickets, ticket)
	return account, nil
}

func runLoginAndTCPConnect(t *testing.T, account string, loginHandler http.Handler, consumer *gateway.Sessions) {
	t.Helper()
	options, err := gateway.SessionOptions()
	if err != nil {
		t.Fatal(err)
	}
	tcpServer, err := orbittcp.NewServer("127.0.0.1:0", options, consumer.Connect, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tcpServer.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := tcpServer.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	loginURL, stopLogin := testkit.StartHTTP(t, loginHandler)
	upstream, err := gateway.NewUpstreamClient()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(upstream.CloseIdleConnections)
	web, err := gateway.NewWebSessions(options, consumer)
	if err != nil {
		t.Fatal(err)
	}
	if err := web.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := web.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	gatewayURL, _ := testkit.StartHTTP(t, gateway.WithWeb(gateway.WithLogin(http.NotFoundHandler(),
		func() []string { return []string{loginURL} }, upstream, tcpServer), web, ""))
	client := &http.Client{Timeout: 4 * time.Second}

	login := func(name, password string, wantStatus int) []byte {
		t.Helper()
		payload, err := proto.Marshal(&sessionpb.LoginRequest{Account: name, Password: password})
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Post(gatewayURL+"/v1/login", "application/protobuf", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != wantStatus {
			t.Fatalf("login status %d, want %d", response.StatusCode, wantStatus)
		}
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	login(account, "wrong-password", http.StatusUnauthorized)
	login("unknown-"+rand.Text(), "test-password", http.StatusUnauthorized)
	var ticket sessionpb.LoginReply
	if err := proto.Unmarshal(login(account, "test-password", http.StatusOK), &ticket); err != nil {
		t.Fatal(err)
	}
	if ticket.GetTicket() == "" || ticket.GetTcpPort() != uint32(tcpServer.Addr().(*net.TCPAddr).Port) {
		t.Fatal("missing login ticket or incorrect TCP port")
	}

	connect := func(value string, version uint32) (rpc.Frame, error) {
		conn, err := net.DialTimeout("tcp", tcpServer.Addr().String(), 3*time.Second)
		if err != nil {
			return rpc.Frame{}, err
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			return rpc.Frame{}, err
		}
		payload, err := proto.Marshal(&sessionpb.ConnectRequest{Ticket: value, ProtocolVersion: version})
		if err != nil {
			return rpc.Frame{}, err
		}
		if err := rpc.Write(conn, rpc.Frame{ID: uint16(sessionpb.SessionMessageId_SESSION_MESSAGE_ID_CONNECT_REQUEST), Sequence: 7, Payload: payload}, options.MaxPayload); err != nil {
			return rpc.Frame{}, err
		}
		return rpc.Read(conn, options.MaxPayload)
	}
	expectClosed := func(value string, version uint32) {
		t.Helper()
		if _, err := connect(value, version); err != io.EOF {
			t.Fatalf("expected gateway to close the rejected connection, got %v", err)
		}
	}
	expectClosed("invalid", 1)
	expectClosed(ticket.GetTicket(), 2)
	type result struct {
		frame rpc.Frame
		err   error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			frame, err := connect(ticket.GetTicket(), 1)
			results <- result{frame, err}
		}()
	}
	var frame rpc.Frame
	successes := 0
	for range 2 {
		outcome := <-results
		if outcome.err == nil {
			frame = outcome.frame
			successes++
		} else if !errors.Is(outcome.err, io.EOF) {
			t.Fatal(outcome.err)
		}
	}
	if successes != 1 {
		t.Fatalf("expected exactly one ticket consumer, got %d", successes)
	}
	if frame.ID != uint16(sessionpb.SessionMessageId_SESSION_MESSAGE_ID_CONNECT_REPLY) || frame.Sequence != 7 {
		t.Fatalf("unexpected reply ID %d or sequence %d", frame.ID, frame.Sequence)
	}
	var reply sessionpb.ConnectReply
	if err := proto.Unmarshal(frame.Payload, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.GetAccount() != account || reply.GetSessionId() == 0 {
		t.Fatal("invalid authenticated session")
	}
	expectClosed(ticket.GetTicket(), 1)
	var webTicket sessionpb.LoginReply
	if err := proto.Unmarshal(login(account, "test-password", http.StatusOK), &webTicket); err != nil {
		t.Fatal(err)
	}
	wsURL, err := url.Parse(gatewayURL + "/v1/session")
	if err != nil {
		t.Fatal(err)
	}
	wsURL.Scheme = "ws"
	header := http.Header{"Origin": []string{gatewayURL}}
	ws, response, err := websocket.DefaultDialer.Dial(wsURL.String(), header)
	if err != nil {
		t.Fatal(err)
	}
	if response != nil {
		_ = response.Body.Close()
	}
	defer ws.Close()
	_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	payload, err := proto.Marshal(&sessionpb.ConnectRequest{Ticket: webTicket.Ticket, ProtocolVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	wire, err := rpc.Marshal(rpc.Frame{ID: 1, Sequence: 7, Payload: payload}, options.MaxPayload)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteMessage(websocket.BinaryMessage, wire); err != nil {
		t.Fatal(err)
	}
	kind, message, err := ws.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if kind != websocket.BinaryMessage {
		t.Fatal("expected binary WebSocket reply")
	}
	wsFrame, err := rpc.Read(bytes.NewReader(message), options.MaxPayload)
	if err != nil {
		t.Fatal(err)
	}
	var webReply sessionpb.ConnectReply
	if err := proto.Unmarshal(wsFrame.Payload, &webReply); err != nil {
		t.Fatal(err)
	}
	if wsFrame.ID != 2 || wsFrame.Sequence != 7 || webReply.Account != account || webReply.SessionId == 0 {
		t.Fatal("invalid WebSocket session")
	}
	expectClosed(webTicket.Ticket, 1)
	header.Set("Origin", "https://another-site.invalid")
	if unexpected, response, err := websocket.DefaultDialer.Dial(wsURL.String(), header); err == nil {
		_ = unexpected.Close()
		t.Fatal("cross-origin WebSocket accepted")
	} else if response != nil {
		_ = response.Body.Close()
	}
	stopLogin()
	login(account, "test-password", http.StatusServiceUnavailable)
}
