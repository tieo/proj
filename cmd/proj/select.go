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
		fmt.Printf("\r\033[K  %s\r\n", header)
	}
	sel := 0
	draw := func(redraw bool) {
		if redraw {
			fmt.Printf("\033[%dA", len(lines))
		}
		for i, ln := range lines {
			marker := "  "
			if i == sel {
				marker = "\033[36m❯\033[0m "
			}
			fmt.Printf("\r\033[K%s%s\r\n", marker, ln)
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

// selectOrCreate is a combobox: a text field (focused by default, for typing a
// new entry) above a list of existing options. ↓ moves into the list, ↑ returns
// to the field. Enter on the field returns the typed text (idx -1); Enter on a
// list item returns that index. ok is false if cancelled (Esc/Ctrl-C).
func selectOrCreate(prompt, defaultText string, options []string) (text string, idx int, ok bool) {
	restore, raw := sttyRaw()
	if !raw {
		return numberedOrCreate(prompt, defaultText, options)
	}
	defer restore()
	fmt.Print("\033[?25l")
	defer fmt.Print("\033[?25h")

	if prompt != "" {
		fmt.Printf("\r\033[K  %s\r\n", prompt)
	}
	total := len(options) + 1 // the input field plus the options
	input := []rune{}
	focus := -1 // -1 = input field, >=0 = option index
	draw := func(redraw bool) {
		if redraw {
			fmt.Printf("\033[%dA", total)
		}
		marker := "  "
		if focus == -1 {
			marker = "\033[36m❯\033[0m "
		}
		var field string
		if len(input) == 0 && defaultText != "" {
			field = "\033[7m \033[0m\033[90m" + defaultText + "\033[0m" // cursor, then greyed placeholder
		} else {
			field = string(input) + "\033[7m \033[0m"
		}
		fmt.Printf("\r\033[K%s\033[90mnew:\033[0m %s\r\n", marker, field)
		for i, opt := range options {
			m := "  "
			if focus == i {
				m = "\033[36m❯\033[0m "
			}
			fmt.Printf("\r\033[K%s%s\r\n", m, opt)
		}
	}
	draw(false)

	buf := make([]byte, 8)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			fmt.Print("\r\n")
			return "", -1, false
		}
		k := buf[:n]
		switch {
		case len(k) >= 3 && k[0] == 27 && k[1] == '[' && k[2] == 'B': // down
			if focus < len(options)-1 {
				focus++
				draw(true)
			}
		case len(k) >= 3 && k[0] == 27 && k[1] == '[' && k[2] == 'A': // up
			if focus >= 0 {
				focus--
				draw(true)
			}
		case len(k) == 1 && (k[0] == '\r' || k[0] == '\n'):
			if focus >= 0 {
				fmt.Print("\r\n")
				return "", focus, true
			}
			t := strings.TrimSpace(string(input))
			if t == "" {
				t = defaultText // Enter on the empty field accepts the placeholder
			}
			if t != "" {
				fmt.Print("\r\n")
				return t, -1, true
			}
		case len(k) == 1 && (k[0] == 3 || k[0] == 27): // Ctrl-C / Esc
			fmt.Print("\r\n")
			return "", -1, false
		case len(k) == 1 && (k[0] == 127 || k[0] == 8): // backspace
			if focus == -1 && len(input) > 0 {
				input = input[:len(input)-1]
				draw(true)
			}
		case focus == -1 && len(k) == 1 && k[0] >= 32 && k[0] < 127:
			input = append(input, rune(k[0]))
			draw(true)
		}
	}
}

// numberedOrCreate is the non-TTY fallback for selectOrCreate: type a name, or a
// number to choose an existing option.
func numberedOrCreate(prompt, defaultText string, options []string) (string, int, bool) {
	if prompt != "" {
		fmt.Println("  " + prompt)
	}
	for i, o := range options {
		fmt.Printf("  %2d  %s\n", i+1, o)
	}
	fmt.Printf("project (new name, or a number) [%s]: ", defaultText)
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return "", -1, false
	}
	in := strings.TrimSpace(sc.Text())
	if in == "" {
		if defaultText != "" {
			return defaultText, -1, true
		}
		return "", -1, false
	}
	if n, err := strconv.Atoi(in); err == nil && n >= 1 && n <= len(options) {
		return "", n - 1, true
	}
	return in, -1, true
}
