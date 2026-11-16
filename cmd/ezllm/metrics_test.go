package main

import (
	"errors"
	"strings"
	"testing"
)

func TestMetricsToken(t *testing.T) {
	files := map[string]string{"/run/scrape": "file-secret-0123456789\nignored\n", "/run/empty": "\n"}
	read := func(p string) ([]byte, error) {
		if s, ok := files[p]; ok {
			return []byte(s), nil
		}
		return nil, errors.New("no such file")
	}
	for _, c := range []struct {
		name      string
		env       map[string]string
		want, err string
	}{
		{name: "unset is off"},
		{name: "variable", env: map[string]string{envMetricsToken: " env-secret-0123456789 "}, want: "env-secret-0123456789"},
		{name: "variable wins over file", env: map[string]string{envMetricsToken: "env-secret-0123456789", envMetricsTokenFile: "/run/scrape"}, want: "env-secret-0123456789"},
		{name: "file first line", env: map[string]string{envMetricsTokenFile: "/run/scrape"}, want: "file-secret-0123456789"},
		{name: "missing file", env: map[string]string{envMetricsTokenFile: "/run/nope"}, err: "no such file"},
		{name: "empty file", env: map[string]string{envMetricsTokenFile: "/run/empty"}, err: "is empty"},
		{name: "short secret", env: map[string]string{envMetricsToken: "short"}, err: "at least 16"},
	} {
		got, err := metricsToken(func(k string) string { return c.env[k] }, read)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: err %v, want %q", c.name, err, c.err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: %q, %v; want %q", c.name, got, err, c.want)
		}
	}
}
