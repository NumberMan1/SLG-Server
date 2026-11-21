package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/NumberMan1/slg-server/internal/loginserver"
)

func loadAdmissionLimits() (loginserver.AdmissionLimits, error) {
	limits := loginserver.DefaultAdmissionLimits()
	for _, option := range []struct {
		key     string
		minimum int
		value   *int
	}{
		{"SLG_LOGIN_CONCURRENCY", 1, &limits.LoginConcurrency},
		{"SLG_REGISTER_CONCURRENCY", 1, &limits.RegistrationConcurrency},
		{"SLG_REGISTER_RATE_PER_SECOND", 0, &limits.RegistrationRate},
	} {
		text := strings.TrimSpace(os.Getenv(option.key))
		if text == "" {
			continue
		}
		value, err := strconv.Atoi(text)
		if err != nil || value < option.minimum {
			return loginserver.AdmissionLimits{}, fmt.Errorf("%s must be an integer >= %d", option.key, option.minimum)
		}
		*option.value = value
	}
	return limits, nil
}
