package logx

import (
	"io"
	"strings"
	"unicode/utf8"
)

// art is "ezllm" in figlet's standard font, unsmushed.
var art = []string{
	`               _  _            `,
	`  ___   ____  | || | _ __ ___  `,
	` / _ \ |_  /  | || || '_ ` + "`" + ` _ \ `,
	`|  __/  / /   | || || | | | | |`,
	` \___| /___|  |_||_||_| |_| |_|`,
}

// codeStyle is the setup code's own look: bold dark-on-yellow, the one place
// in ezllm's output with a filled background, so it cannot be mistaken for
// a log value.
const codeStyle = "\x1b[1;30;43m"

// boxLine is one row of a panel: its plain text (for width) and how to print it.
type boxLine struct{ plain, styled string }

func line(plain string) boxLine { return boxLine{plain, plain} }

// spacedCode renders ABCD-EFGH as "A B C D - E F G H": easier to read off a
// terminal and copy by eye.
func spacedCode(code string) string {
	return strings.Join(strings.Split(code, ""), " ")
}

// panel frames lines in a double-line box. Widths count runes of the plain
// text, so color escapes never skew the right edge.
func panel(w *strings.Builder, color bool, lines []boxLine) {
	width := 0
	for _, l := range lines {
		if n := utf8.RuneCountInString(l.plain); n > width {
			width = n
		}
	}
	edge := func(s string) string {
		if !color {
			return s
		}
		return bold + yellow + s + reset
	}
	w.WriteString(edge("╔"+strings.Repeat("═", width+4)+"╗") + "\n")
	for _, l := range lines {
		pad := width - utf8.RuneCountInString(l.plain)
		w.WriteString(edge("║") + "  " + l.styled + strings.Repeat(" ", pad) + "  " + edge("║") + "\n")
	}
	w.WriteString(edge("╚"+strings.Repeat("═", width+4)+"╝") + "\n")
}

func codeLine(color bool, code string) boxLine {
	plain := " " + spacedCode(code) + " "
	styled := plain
	if color {
		styled = codeStyle + plain + reset
	}
	return boxLine{"setup code  " + plain, "setup code  " + styled}
}

// FirstBootBanner prints the first-boot screen: the ezllm art over a framed
// panel with the dashboard URL and the one-time setup code. It writes
// straight to w (not through slog): it is for a person at a terminal.
func FirstBootBanner(w io.Writer, color bool, url, code string) {
	p := func(c, s string) string {
		if !color {
			return s
		}
		return c + s + reset
	}
	var b strings.Builder
	b.WriteString("\n")
	for _, l := range art {
		b.WriteString(p(bold+cyan, l) + "\n")
	}
	b.WriteString("\n")
	title := "First boot: set the dashboard password"
	panel(&b, color, []boxLine{
		{title, p(bold, title)},
		line(""),
		{"open        " + url, "open        " + p(cyan, url)},
		codeLine(color, code),
		line(""),
		{"The code works once and changes on every restart until setup is done.",
			p(dim, "The code works once and changes on every restart until setup is done.")},
		{"To skip setup, set EZLLM_DASHBOARD_PASSWORD (or _FILE) and restart.",
			p(dim, "To skip setup, set EZLLM_DASHBOARD_PASSWORD (or _FILE) and restart.")},
	})
	b.WriteString("\n")
	_, _ = io.WriteString(w, b.String())
}
