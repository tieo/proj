package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// selectFromList renders a header and lines with a moving ❯ cursor, returning
// the chosen index or -1 if cancelled. Up/Down (or k/j) move, Enter selects, q
// or Esc cancels. When stdin is not an interactive terminal it falls back to a
// numbered prompt.
func selectFromList(header string, lines []string) int {
	if len(lines) == 0 {
		return -1
	}
	restore, ok := sttyRaw()
	if !ok {
		return numberedSelect(header, lines)
	}
	defer restore()
	fmt.Print("\033[?25l")       // hide the cursor while navigating
	defer fmt.Print("\033[?25h") // restore it on exit

	if header != "" {
		fmt.Printf("\r\033[K     %s\r\n", header)
	}
	sel := 0
	draw := func(redraw bool) {
		if redraw {
			fmt.Printf("\033[%dA", len(lines))
		}
		for i, ln := range lines {
			marker := " "
			if i == sel {
				marker = "\033[36m❯\033[0m"
			}
			fmt.Printf("\r\033[K%s %2d %s\r\n", marker, i+1, ln)
		}
	}
	draw(false)

	buf := make([]byte, 8)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			fmt.Print("\r\n")
			return -1
		}
		k := buf[:n]
		up := (len(k) >= 3 && k[0] == 27 && k[1] == '[' && k[2] == 'A') || (len(k) == 1 && k[0] == 'k')
		down := (len(k) >= 3 && k[0] == 27 && k[1] == '[' && k[2] == 'B') || (len(k) == 1 && k[0] == 'j')
		switch {
		case up:
			if sel > 0 {
				sel--
			}
			draw(true)
		case down:
			if sel < len(lines)-1 {
				sel++
			}
			draw(true)
		case len(k) == 1 && (k[0] == '\r' || k[0] == '\n'):
			fmt.Print("\r\n")
			return sel
		case len(k) == 1 && (k[0] == 'q' || k[0] == 3 || k[0] == 27):
			fmt.Print("\r\n")
			return -1
		}
	}
}

// sttyRaw puts the controlling terminal into raw, no-echo mode and returns a
// restore func. ok is false when stdin is not a usable terminal.
func sttyRaw() (restore func(), ok bool) {
	g := exec.Command("stty", "-g")
	g.Stdin = os.Stdin
	saved, err := g.Output()
	if err != nil {
		return func() {}, false
	}
	raw := exec.Command("stty", "raw", "-echo")
	raw.Stdin = os.Stdin
	if err := raw.Run(); err != nil {
		return func() {}, false
	}
	return func() {
		c := exec.Command("stty", strings.TrimSpace(string(saved)))
		c.Stdin = os.Stdin
		_ = c.Run()
	}, true
}

// numberedSelect is the non-interactive fallback used when stdin is not a TTY.
func numberedSelect(header string, lines []string) int {
	if header != "" {
		fmt.Printf("     %s\n", header)
	}
	for i, ln := range lines {
		fmt.Printf("  %2d %s\n", i+1, ln)
	}
	fmt.Printf("select? [1-%d, q to cancel]: ", len(lines))
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return -1
	}
	in := strings.TrimSpace(sc.Text())
	if in == "" || in == "q" {
		return -1
	}
	i, err := strconv.Atoi(in)
	if err != nil || i < 1 || i > len(lines) {
		return -1
	}
	return i - 1
}
