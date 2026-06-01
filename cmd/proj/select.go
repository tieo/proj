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

// fName and fTags are the two field "focus" values; focus >= 0 is an option.
const (
	fName = -2
	fTags = -1
)

// selectOrCreate is a combobox for choosing or creating a project. The top line
// holds a name field and a tags field; the existing projects are listed below.
// On the name field, space/tab/enter jump to tags; on the tags field, enter
// confirms and tab returns to the name. ↓ enters the project list and ↑ leaves
// it. It returns a typed name and tags (idx -1) to create, or an option index to
// pick an existing project. ok is false if cancelled (Esc/Ctrl-C).
func selectOrCreate(prompt, defaultName string, options []string) (name string, tags []string, idx int, ok bool) {
	restore, raw := sttyRaw()
	if !raw {
		return numberedOrCreate(prompt, defaultName, options)
	}
	defer restore()
	defer fmt.Print("\033[?25h")

	if prompt != "" {
		fmt.Printf("\r\033[K  %s\r\n", prompt)
	}
	total := len(options) + 1 // input line + options
	var nm, tg []rune
	focus := fName

	// seg renders a field's text, or a greyed placeholder when empty, with its
	// visible width (used to place the real cursor).
	seg := func(typed []rune, placeholder string) (string, int) {
		if len(typed) > 0 {
			return string(typed), len(typed)
		}
		return "\033[90m" + placeholder + "\033[0m", len([]rune(placeholder))
	}

	draw := func() {
		fmt.Print("\033[?25l\r")
		nameText, nameW := seg(nm, defaultName)
		tagsText, _ := seg(tg, "tags")
		marker := "  "
		if focus < 0 {
			marker = "\033[36m❯\033[0m "
		}
		fmt.Printf("\033[K%s%s  %s\r\n", marker, nameText, tagsText)
		for i, opt := range options {
			m := "  "
			if focus == i {
				m = "\033[36m❯\033[0m "
			}
			fmt.Printf("\033[K%s%s\r\n", m, opt)
		}
		fmt.Printf("\033[%dA\r", total) // back to the input line, column 0
		if focus < 0 {
			col := 2 + len(nm)
			if focus == fTags {
				col = 2 + nameW + 2 + len(tg)
			}
			fmt.Printf("\033[%dC\033[?25h", col) // place the real (blinking) cursor
		}
	}
	draw()

	buf := make([]byte, 8)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			fmt.Print("\r\n")
			return "", nil, -1, false
		}
		k := buf[:n]
		switch {
		case len(k) >= 3 && k[0] == 27 && k[1] == '[' && k[2] == 'B': // down
			if len(options) > 0 {
				if focus < 0 {
					focus = 0
				} else if focus < len(options)-1 {
					focus++
				}
				draw()
			}
		case len(k) >= 3 && k[0] == 27 && k[1] == '[' && k[2] == 'A': // up
			if focus == 0 || focus == fTags {
				focus = fName
				draw()
			} else if focus > 0 {
				focus--
				draw()
			}
		case len(k) == 1 && (k[0] == 3 || (k[0] == 27 && n == 1)): // Ctrl-C / Esc
			fmt.Print("\r\n")
			return "", nil, -1, false
		case focus >= 0 && len(k) == 1 && (k[0] == '\r' || k[0] == '\n'):
			fmt.Print("\r\n")
			return "", nil, focus, true
		case focus == fName:
			switch {
			case len(k) == 1 && (k[0] == ' ' || k[0] == '\t' || k[0] == '\r' || k[0] == '\n'):
				focus = fTags
				draw()
			case len(k) == 1 && (k[0] == 127 || k[0] == 8):
				if len(nm) > 0 {
					nm = nm[:len(nm)-1]
					draw()
				}
			case len(k) == 1 && k[0] >= 32 && k[0] < 127:
				nm = append(nm, rune(k[0]))
				draw()
			}
		case focus == fTags:
			switch {
			case len(k) == 1 && (k[0] == '\r' || k[0] == '\n'):
				name = strings.TrimSpace(string(nm))
				if name == "" {
					name = defaultName
				}
				if name == "" {
					break
				}
				fmt.Print("\r\n")
				return name, strings.Fields(string(tg)), -1, true
			case len(k) == 1 && k[0] == '\t':
				focus = fName
				draw()
			case len(k) == 1 && (k[0] == 127 || k[0] == 8):
				if len(tg) > 0 {
					tg = tg[:len(tg)-1]
				} else {
					focus = fName
				}
				draw()
			case len(k) == 1 && k[0] >= 32 && k[0] < 127:
				tg = append(tg, rune(k[0]))
				draw()
			}
		}
	}
}

// numberedOrCreate is the non-TTY fallback for selectOrCreate.
func numberedOrCreate(prompt, defaultName string, options []string) (string, []string, int, bool) {
	if prompt != "" {
		fmt.Println("  " + prompt)
	}
	for i, o := range options {
		fmt.Printf("  %2d  %s\n", i+1, o)
	}
	fmt.Printf("project [%s] (name [tags...], or a number): ", defaultName)
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return "", nil, -1, false
	}
	in := strings.TrimSpace(sc.Text())
	if in == "" {
		if defaultName != "" {
			return defaultName, nil, -1, true
		}
		return "", nil, -1, false
	}
	if n, err := strconv.Atoi(in); err == nil && n >= 1 && n <= len(options) {
		return "", nil, n - 1, true
	}
	f := strings.Fields(in)
	return f[0], f[1:], -1, true
}
