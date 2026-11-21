package main

import (
	"strings"
	"testing"

	"github.com/NumberMan1/slg-server/internal/loginserver"
)

func TestAdmissionLimitsFromEnv(t *testing.T) {
	t.Setenv("SLG_LOGIN_CONCURRENCY", "")
	t.Setenv("SLG_REGISTER_CONCURRENCY", "")
	t.Setenv("SLG_REGISTER_RATE_PER_SECOND", "")
	defaults, err := loadAdmissionLimits()
	if err != nil || defaults != loginserver.DefaultAdmissionLimits() {
		t.Fatalf("unexpected default limits: %v, %v", defaults, err)
	}
	t.Setenv("SLG_LOGIN_CONCURRENCY", "48")
	t.Setenv("SLG_REGISTER_CONCURRENCY", "12")
	t.Setenv("SLG_REGISTER_RATE_PER_SECOND", "20")
	limits, err := loadAdmissionLimits()
	want := loginserver.AdmissionLimits{LoginConcurrency: 48, RegistrationConcurrency: 12, RegistrationRate: 20}
	if err != nil || limits != want {
		t.Fatalf("unexpected configured limits: %v, %v", limits, err)
	}
	for _, tc := range []struct{ key, value string }{
		{"SLG_LOGIN_CONCURRENCY", "invalid"},
		{"SLG_REGISTER_CONCURRENCY", "0"},
		{"SLG_REGISTER_RATE_PER_SECOND", "-1"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if _, err := loadAdmissionLimits(); err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("expected an error for %s, got %v", tc.key, err)
			}
		})
	}
}
