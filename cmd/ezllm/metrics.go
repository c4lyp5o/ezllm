package main

import (
	"fmt"

	"strings"
)

const (
	envMetricsToken     = "EZLLM_METRICS_TOKEN"
	envMetricsTokenFile = "EZLLM_METRICS_TOKEN_FILE"
	minMetricsToken     = 16
)

// metricsToken reads the /metrics scrape secret: the variable, else the first
// line of the file it names. Empty means metrics are off. A short secret is
// refused at boot rather than guarded by a guessable value.
func metricsToken(getenv func(string) string, readFile func(string) ([]byte, error)) (string, error) {
	tok := strings.TrimSpace(getenv(envMetricsToken))
	if tok == "" {
		path := strings.TrimSpace(getenv(envMetricsTokenFile))
		if path == "" {
			return "", nil
		}
		raw, err := readFile(path)
		if err != nil {
			return "", fmt.Errorf("%s: %w", envMetricsTokenFile, err)
		}
		tok, _, _ = strings.Cut(string(raw), "\n")
		tok = strings.TrimSpace(tok)
		if tok == "" {
			return "", fmt.Errorf("%s: %s is empty", envMetricsTokenFile, path)
		}
	}
	if len(tok) < minMetricsToken {
		return "", fmt.Errorf("metrics token must be at least %d characters", minMetricsToken)
	}
	return tok, nil
}
