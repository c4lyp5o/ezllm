package logx

import (
	"io"
	"strings"
)

// art is "ezllm" in figlet's standard font, unsmushed.
var art = []string{
	`               _  _            `,
	`  ___   ____  | || | _ __ ___  `,
	` / _ \ |_  /  | || || '_ ` + "`" + ` _ \ `,
	`|  __/  / /   | || || | | | | |`,
	` \___| /___|  |_||_||_| |_| |_|`,
}

// FirstBootBanner prints the first-boot screen: the ezllm art, where to
// open the dashboard, and the one-time setup code the set-password page
// asks for. It writes straight to w (not through slog) because it is meant
// to be read by a person at a terminal, once.
func FirstBootBanner(w io.Writer, color bool, url, code string) {
	p := func(c, s string) string {
		if !color {
			return s
		}
		return c + s + reset
	}
	var b strings.Builder
	b.WriteString("\n")
	for _, line := range art {
		b.WriteString(p(bold+cyan, line) + "\n")
	}
	b.WriteString("\n")
	b.WriteString("  " + p(bold, "First boot: set the dashboard password") + "\n\n")
	b.WriteString("  open        " + p(cyan, url) + "\n")
	b.WriteString("  setup code  " + p(bold+yellow, code) + "\n\n")
	b.WriteString(p(dim, "  The code works once and changes on every restart until setup is done.") + "\n")
	b.WriteString(p(dim, "  To skip setup, set EZLLM_DASHBOARD_PASSWORD (or _FILE) and restart.") + "\n\n")
	_, _ = io.WriteString(w, b.String())
}
