//go:build linux

package main

import (
	"errors"
	"os"
)

// envDefault preserves an explicit empty value, which disables optional integrations.
func envDefault(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

// validateLogin fails before database or filesystem effects when the build does not provide IDaaS.
func validateLogin(login string, fixtures bool) error {
	switch login {
	case "email":
		return nil
	case "idaas":
		if fixtures {
			return errors.New("IDaaS deployment forbids cleanup and fixture flags")
		}
		return errors.New("this build does not provide IDaaS login; deployment startup is unavailable and must not fall back to email")
	default:
		return errors.New("V2_LOGIN must be explicitly set to email (development) or idaas (deployment)")
	}
}
