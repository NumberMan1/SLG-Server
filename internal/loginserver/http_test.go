package loginserver

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	serverlog "github.com/NumberMan1/log"
	"google.golang.org/protobuf/proto"

	pb "github.com/NumberMan1/slg-server/proto"
)

type accountStub struct {
	register func(context.Context) error
}

func (a accountStub) Register(ctx context.Context, _, _ string) error {
	if a.register != nil {
		return a.register(ctx)
	}
	return nil
}

func (accountStub) Authenticate(context.Context, string, string) error { return nil }

type testTicketIssuer struct{}

func (testTicketIssuer) Issue(context.Context, string) (string, error) { return "test-ticket", nil }

func postCredentials(t *testing.T, handler http.Handler, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, bytes.NewReader(body))
	request.Header.Set("Content-Type", protobufType)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestRegistrationCapacityReservesLogin(t *testing.T) {
	limits := DefaultAdmissionLimits()
	limits.RegistrationConcurrency = 2
	started := make(chan struct{}, limits.RegistrationConcurrency)
	finish := make(chan struct{})
	accounts := accountStub{register: func(ctx context.Context) error {
		started <- struct{}{}
		select {
		case <-finish:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	var registrations sync.WaitGroup
	defer func() {
		close(finish)
		registrations.Wait()
	}()
	handler := NewHandler(accounts, testTicketIssuer{}, serverlog.DefaultLogger(), limits)
	register, err := proto.Marshal(&pb.RegisterRequest{Account: "test", Password: "test-password"})
	if err != nil {
		t.Fatal(err)
	}
	for range limits.RegistrationConcurrency {
		registrations.Go(func() { postCredentials(t, handler, "/internal/v1/register", register) })
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("registration did not acquire its slot")
		}
	}
	if status := postCredentials(t, handler, "/internal/v1/register", register).Code; status != http.StatusServiceUnavailable {
		t.Fatalf("registration exceeded its concurrency limit: %d", status)
	}
	login, err := proto.Marshal(&pb.LoginRequest{Account: "test", Password: "test-password"})
	if err != nil {
		t.Fatal(err)
	}
	if status := postCredentials(t, handler, "/internal/v1/login", login).Code; status != http.StatusOK {
		t.Fatalf("registration saturation blocked login: %d", status)
	}
}

func TestRegistrationRateResetsNextSecond(t *testing.T) {
	rate := registrationLimit{perSecond: 3}
	now := time.Now()
	for range 3 {
		if !rate.allow(now) {
			t.Fatal("registration was limited before the window filled")
		}
	}
	if rate.allow(now.Add(999*time.Millisecond)) || !rate.allow(now.Add(time.Second)) {
		t.Fatal("registration limit did not reset at the next window")
	}
}

func TestRegistrationRateIsOptIn(t *testing.T) {
	register, err := proto.Marshal(&pb.RegisterRequest{Account: "test", Password: "test-password"})
	if err != nil {
		t.Fatal(err)
	}
	limits := DefaultAdmissionLimits()
	unlimited := NewHandler(accountStub{}, testTicketIssuer{}, serverlog.DefaultLogger(), limits)
	for range 12 {
		if status := postCredentials(t, unlimited, "/internal/v1/register", register).Code; status != http.StatusCreated {
			t.Fatalf("default registration rate unexpectedly rejected request: %d", status)
		}
	}
	limits.RegistrationRate = 1
	limited := NewHandler(accountStub{}, testTicketIssuer{}, serverlog.DefaultLogger(), limits)
	if status := postCredentials(t, limited, "/internal/v1/register", register).Code; status != http.StatusCreated {
		t.Fatalf("first registration was rejected: %d", status)
	}
	response := postCredentials(t, limited, "/internal/v1/register", register)
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "1" {
		t.Fatalf("registration rate returned %d, Retry-After=%q", response.Code, response.Header().Get("Retry-After"))
	}
}
