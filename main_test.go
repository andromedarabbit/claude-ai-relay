package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAuthRetryRespectsNoCLIRefresh(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "model-called")
	t.Setenv("PATH", dir)
	t.Setenv("MARKER", marker)
	for name, script := range map[string]string{
		"security": "#!/bin/sh\nexit 1\n",
		"claude":   "#!/bin/sh\nprintf called > \"$MARKER\"\nexit 1\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	previous := noCLIRefresh
	t.Cleanup(func() { noCLIRefresh = previous })
	for _, disabled := range []bool{true, false} {
		noCLIRefresh = disabled
		_, _, err := withAuthRetry(credentials{AccessToken: "fixture"}, func(credentials) (int, error) {
			return 0, &RelayError{Kind: "auth", Detail: "401"}
		})
		var relayErr *RelayError
		if !errors.As(err, &relayErr) || relayErr.Kind != "auth" {
			t.Fatalf("expected auth error, got %v", err)
		}
		_, err = os.Stat(marker)
		if (disabled && !os.IsNotExist(err)) || (!disabled && err != nil) {
			t.Fatalf("unexpected CLI launch with disabled=%v: %v", disabled, err)
		}
	}
}
