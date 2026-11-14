package testcases

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/NumberMan1/orbit/rpc"
	orbittcp "github.com/NumberMan1/orbit/socket/tcp"
	"google.golang.org/protobuf/proto"

	"github.com/NumberMan1/slg-server/internal/gateway"
	sessionpb "github.com/NumberMan1/slg-server/proto"
)

func TestLoginAndTCPConnect(t *testing.T) {
	sessions := gateway.NewSessions("demo", "test-password")
	options, err := gateway.SessionOptions()
	if err != nil {
		t.Fatal(err)
	}
	tcpServer, err := orbittcp.NewServer("127.0.0.1:0", options, sessions.Connect, nil)
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
	httpServer := httptest.NewServer(gateway.WithLogin(http.NotFoundHandler(), sessions, tcpServer))
	t.Cleanup(httpServer.Close)
	client := &http.Client{Timeout: 3 * time.Second}

	login := func(password string, wantStatus int) []byte {
		t.Helper()
		payload, err := proto.Marshal(&sessionpb.LoginRequest{Account: "demo", Password: password})
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Post(httpServer.URL+"/v1/login", "application/protobuf", bytes.NewReader(payload))
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
	login("wrong-password", http.StatusUnauthorized)
	var ticket sessionpb.LoginReply
	if err := proto.Unmarshal(login("test-password", http.StatusOK), &ticket); err != nil {
		t.Fatal(err)
	}
	if ticket.GetTicket() == "" || ticket.GetTcpPort() != uint32(tcpServer.Addr().(*net.TCPAddr).Port) {
		t.Fatal("missing login ticket or incorrect TCP port")
	}

	connect := func(value string, version uint32) (rpc.Frame, error) {
		t.Helper()
		conn, err := net.DialTimeout("tcp", tcpServer.Addr().String(), 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		payload, err := proto.Marshal(&sessionpb.ConnectRequest{Ticket: value, ProtocolVersion: version})
		if err != nil {
			t.Fatal(err)
		}
		if err := rpc.Write(conn, rpc.Frame{ID: uint16(sessionpb.SessionMessageId_SESSION_MESSAGE_ID_CONNECT_REQUEST), Sequence: 7, Payload: payload}, options.MaxPayload); err != nil {
			t.Fatal(err)
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
	frame, err := connect(ticket.GetTicket(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if frame.ID != uint16(sessionpb.SessionMessageId_SESSION_MESSAGE_ID_CONNECT_REPLY) || frame.Sequence != 7 {
		t.Fatalf("unexpected reply ID %d or sequence %d", frame.ID, frame.Sequence)
	}
	var reply sessionpb.ConnectReply
	if err := proto.Unmarshal(frame.Payload, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.GetAccount() != "demo" || reply.GetSessionId() == 0 {
		t.Fatal("invalid authenticated session")
	}
	expectClosed(ticket.GetTicket(), 1)
}
