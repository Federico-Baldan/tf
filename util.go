package main

import (
	"fmt"
	"strings"
	"time"
	"unicode"
)

// shorten compresses a resource address to fit max display cells:
// module. → m., then middle-ellipsis if still too long.
func shorten(addr string, n int) string {
	n = max(n, 8)
	s := strings.ReplaceAll(clean(addr), "module.", "m.")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	head := n / 3
	tail := n - head - 1
	return string(r[:head]) + "…" + string(r[len(r)-tail:])
}

func fmtDur(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func truncRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// clean replaces control characters (including ESC and the C1 range) with
// U+FFFD. Names, ids and messages come from configs and providers; printed
// raw they could move the cursor, clear the screen or retitle the window.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' {
			return '\uFFFD'
		}
		return r
	}, s)
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}
