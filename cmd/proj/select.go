package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

var ansiSeq = regexp.MustCompile("\x1b\\[[0-9;]*m")

// visibleWidth is the rune count of s ignoring ANSI color sequences.
func visibleWidth(s string) int {
	return len([]rune(ansiSeq.ReplaceAllString(s, "")))
}

// highlightRow draws a subtle full-width background bar behind a selected row.
// The bar is re-armed after every reset the row emits, so the per-cell colors
// (green project, dimmed answer) keep their own foreground while sitting on it.
func highlightRow(s string) string {
	const bg = "\033[48;5;237m"
	body := strings.ReplaceAll(s, "\033[0m", "\033[0m"+bg)
	if pad := termWidth() - 1 - visibleWidth(s); pad > 0 {
		body += strings.Repeat(" ", pad)
	}
	return bg + body + "\033[0m"
}

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
	renderRow := func(i int) string {
		marker := "  "
		row := marker + lines[i]
		if i == sel {
			marker = "\033[36m❯\033[0m "
			row = highlightRow(marker + lines[i])
		}
		return row
	}
	// Initial paint: each row, cursor ends one line below the last row ("home").
	for i := range lines {
		fmt.Printf("\r\033[K%s\r\n", renderRow(i))
	}
	// repaint one row in place. Repainting only the row losing the cursor and
	// the row gaining it (instead of redrawing every row on every keypress)
	// kills the flicker on unrelated lines.
	repaint := func(i int) {
		up := len(lines) - i
		fmt.Printf("\033[%dA\r\033[K%s\r\n", up, renderRow(i))
		if down := len(lines) - i - 1; down > 0 {
			fmt.Printf("\033[%dB", down)
		}
	}

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
				prev := sel
				sel--
				repaint(prev)
				repaint(sel)
			}
		case down:
			if sel < len(lines)-1 {
				prev := sel
				sel++
				repaint(prev)
				repaint(sel)
			}
		case len(k) == 1 && (k[0] == '\r' || k[0] == '\n'):
			fmt.Print("\r\n")
			return sel
		case len(k) == 1 && (k[0] == 'q' || k[0] == 3 || k[0] == 27):
			fmt.Print("\r\n")
			return -1
		}
	}
}

// selectAction renders a header, rows, and a key-hint footer with a moving
// cursor, returning the chosen row index and the action key pressed. Up/Down
// (or k/j) move; Enter and each rune in actions return (index, key); q or Esc
// return (-1, 0). Non-interactive stdin falls back to a numbered pick that only
// supports Enter (returns index, '\r'). It is selectFromList plus custom action
// keys, for a list where a row can be acted on several ways.
func selectAction(header string, lines []string, footer string, actions string) (int, byte) {
	if len(lines) == 0 {
		return -1, 0
	}
	restore, ok := sttyRaw()
	if !ok {
		return numberedSelect(header, lines), '\r'
	}
	defer restore()
	fmt.Print("\033[?25l")
	defer fmt.Print("\033[?25h")

	if header != "" {
		fmt.Printf("\r\033[K  %s\r\n", header)
	}
	sel := 0
	renderRow := func(i int) string {
		marker := "  "
		row := marker + lines[i]
		if i == sel {
			marker = "\033[36m❯\033[0m "
			row = highlightRow(marker + lines[i])
		}
		return row
	}
	for i := range lines {
		fmt.Printf("\r\033[K%s\r\n", renderRow(i))
	}
	if footer != "" {
		fmt.Printf("\r\033[K  \033[90m%s\033[0m\r\n", footer)
	}
	// The footer occupies one line below the rows; account for it when moving
	// the cursor back up to repaint a row in place.
	footerLines := 0
	if footer != "" {
		footerLines = 1
	}
	repaint := func(i int) {
		up := len(lines) - i + footerLines
		fmt.Printf("\033[%dA\r\033[K%s\r\n", up, renderRow(i))
		if down := len(lines) - i - 1 + footerLines; down > 0 {
			fmt.Printf("\033[%dB", down)
		}
	}

	buf := make([]byte, 8)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			fmt.Print("\r\n")
			return -1, 0
		}
		k := buf[:n]
		up := (len(k) >= 3 && k[0] == 27 && k[1] == '[' && k[2] == 'A') || (len(k) == 1 && k[0] == 'k')
		down := (len(k) >= 3 && k[0] == 27 && k[1] == '[' && k[2] == 'B') || (len(k) == 1 && k[0] == 'j')
		switch {
		case up:
			if sel > 0 {
				prev := sel
				sel--
				repaint(prev)
				repaint(sel)
			}
		case down:
			if sel < len(lines)-1 {
				prev := sel
				sel++
				repaint(prev)
				repaint(sel)
			}
		case len(k) == 1 && (k[0] == '\r' || k[0] == '\n'):
			fmt.Print("\r\n")
			return sel, '\r'
		case len(k) == 1 && (k[0] == 'q' || k[0] == 3 || k[0] == 27):
			fmt.Print("\r\n")
			return -1, 0
		case len(k) == 1 && strings.IndexByte(actions, k[0]) >= 0:
			fmt.Print("\r\n")
			return sel, k[0]
		}
	}
}

// selectFromEnd renders a scrolling viewport of at most height rows over lines,
// with the cursor starting on the last line (the newest message), for choosing
// from a long list. Up/Down move and scroll one row; PageUp/PageDown move a
// screenful; Home/End jump to the ends; Enter returns the index; q or Esc return
// -1. Only the viewport is drawn, so a thousand-line list costs the same as a
// short one. Non-interactive stdin falls back to a numbered prompt.
func selectFromEnd(header string, lines []string, footer string, height int) int {
	n := len(lines)
	if n == 0 {
		return -1
	}
	if height > n {
		height = n
	}
	if height < 1 {
		height = 1
	}
	restore, ok := sttyRaw()
	if !ok {
		return numberedSelect(header, lines)
	}
	defer restore()
	fmt.Print("\033[?25l")
	defer fmt.Print("\033[?25h")

	sel := n - 1
	top := n - height
	clampTop := func() {
		if sel < top {
			top = sel
		}
		if sel >= top+height {
			top = sel - height + 1
		}
		if top > n-height {
			top = n - height
		}
		if top < 0 {
			top = 0
		}
	}
	clampTop()

	frame := func() string {
		var b strings.Builder
		b.WriteString("\r\033[K  " + header + "\r\n")
		for i := top; i < top+height; i++ {
			b.WriteString("\r\033[K")
			if i == sel {
				b.WriteString(highlightRow("\033[36m❯\033[0m " + lines[i]))
			} else {
				b.WriteString("  " + lines[i])
			}
			b.WriteString("\r\n")
		}
		b.WriteString(fmt.Sprintf("\r\033[K  \033[90m%s   %d/%d\033[0m\r\n", footer, sel+1, n))
		return b.String()
	}
	fmt.Print(frame())
	total := height + 2 // header + rows + footer
	repaint := func() {
		fmt.Printf("\033[%dA", total)
		fmt.Print(frame())
	}

	buf := make([]byte, 8)
	for {
		nr, err := os.Stdin.Read(buf)
		if err != nil || nr == 0 {
			fmt.Print("\r\n")
			return -1
		}
		k := buf[:nr]
		esc := len(k) >= 3 && k[0] == 27 && k[1] == '['
		switch {
		case (esc && k[2] == 'A') || (nr == 1 && k[0] == 'k'): // up
			if sel > 0 {
				sel--
				clampTop()
				repaint()
			}
		case (esc && k[2] == 'B') || (nr == 1 && k[0] == 'j'): // down
			if sel < n-1 {
				sel++
				clampTop()
				repaint()
			}
		case esc && k[2] == '5': // PageUp
			sel -= height
			if sel < 0 {
				sel = 0
			}
			clampTop()
			repaint()
		case esc && k[2] == '6': // PageDown
			sel += height
			if sel > n-1 {
				sel = n - 1
			}
			clampTop()
			repaint()
		case (esc && (k[2] == 'H' || k[2] == '1')) || (nr == 1 && k[0] == 'g'): // Home
			sel = 0
			clampTop()
			repaint()
		case (esc && (k[2] == 'F' || k[2] == '4')) || (nr == 1 && k[0] == 'G'): // End
			sel = n - 1
			clampTop()
			repaint()
		case nr == 1 && (k[0] == '\r' || k[0] == '\n'):
			fmt.Print("\r\n")
			return sel
		case nr == 1 && (k[0] == 'q' || k[0] == 3 || k[0] == 27):
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
	fName       = -2
	fTags       = -1
	projNameCol = 16 // width of the NAME column, shared with pickProject
)

// selectOrCreate is a combobox for choosing or creating a project: a NAME field
// and a TAGS field on the top line, existing projects below. On NAME,
// space/tab/enter jump to TAGS; on TAGS, enter creates the project and Shift+Tab
// goes back to NAME. ↓ enters the project list, ↑ leaves it. Returns a typed name
// and tags (idx -1) to create, or an option index to pick an existing project.
func selectOrCreate(defaultName string, options []string) (name string, tags []string, idx int, ok bool) {
	restore, raw := sttyRaw()
	if !raw {
		return numberedOrCreate(defaultName, options)
	}
	defer restore()
	defer fmt.Print("\033[?25h")

	fmt.Printf("\r\033[K  \033[90m%-*s  %s\033[0m\r\n", projNameCol, "NAME", "TAGS")
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

	// inputLine renders the editable line (NAME + TAGS) with the current focus
	// marker, and returns the column width chosen for the NAME field so the
	// caller can place the real cursor on the TAGS side.
	inputLine := func() (string, int) {
		nameText, nameW := seg(nm, defaultName)
		tagsText, _ := seg(tg, "tags")
		colW := projNameCol
		if nameW > colW {
			colW = nameW
		}
		nameField := nameText + strings.Repeat(" ", colW-nameW)
		marker := "  "
		if focus < 0 {
			marker = "\033[36m❯\033[0m "
		}
		return fmt.Sprintf("%s%s  %s", marker, nameField, tagsText), colW
	}

	// optionRow renders option i with current focus highlight.
	optionRow := func(i int) string {
		m := "  "
		row := m + options[i]
		if focus == i {
			m = "\033[36m❯\033[0m "
			row = highlightRow(m + options[i])
		}
		return row
	}

	// repaintInput rewrites just the input line in place; positions the real
	// (blinking) cursor in the focused field, or hides it if focus moved off.
	repaintInput := func() {
		line, colW := inputLine()
		var b strings.Builder
		b.WriteString("\033[?25l\r\033[K")
		b.WriteString(line)
		b.WriteString("\r")
		switch focus {
		case fName:
			fmt.Fprintf(&b, "\033[%dC\033[?25h", 2+len(nm))
		case fTags:
			fmt.Fprintf(&b, "\033[%dC\033[?25h", 2+colW+2+len(tg))
		}
		fmt.Print(b.String())
	}

	// repaintOption rewrites just option row i in place; cursor returns to the
	// input row's column 0 and stays hidden (focus is on the list).
	repaintOption := func(i int) {
		var b strings.Builder
		fmt.Fprintf(&b, "\033[%dB\r\033[K", i+1)
		b.WriteString(optionRow(i))
		fmt.Fprintf(&b, "\033[%dA\r", i+1)
		fmt.Print(b.String())
	}

	// Initial paint: input line + every option, then cursor returns to the
	// input row and the field-cursor (or hide) is applied by repaintInput.
	{
		line, _ := inputLine()
		var b strings.Builder
		b.WriteString("\033[?25l\r\033[K")
		b.WriteString(line)
		b.WriteString("\r\n")
		for i := range options {
			b.WriteString("\r\033[K")
			b.WriteString(optionRow(i))
			b.WriteString("\r\n")
		}
		fmt.Fprintf(&b, "\033[%dA", total)
		fmt.Print(b.String())
		repaintInput()
	}

	// clearWidget wipes the whole combobox (header line down) so the caller's
	// output replaces it instead of overlapping leftover option text.
	clearWidget := func() {
		fmt.Print("\033[1A\r\033[J\033[?25h")
	}

	buf := make([]byte, 8)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			clearWidget()
			return "", nil, -1, false
		}
		k := buf[:n]
		switch {
		case len(k) >= 3 && k[0] == 27 && k[1] == '[' && k[2] == 'B': // down
			if len(options) > 0 {
				prev := focus
				if focus < 0 {
					focus = 0
				} else if focus < len(options)-1 {
					focus++
				}
				if prev != focus {
					if prev < 0 {
						repaintInput()
					} else {
						repaintOption(prev)
					}
					repaintOption(focus)
				}
			}
		case len(k) >= 3 && k[0] == 27 && k[1] == '[' && k[2] == 'A': // up
			prev := focus
			if focus == 0 || focus == fTags {
				focus = fName
			} else if focus > 0 {
				focus--
			}
			if prev != focus {
				if prev >= 0 {
					repaintOption(prev)
				}
				if focus < 0 {
					repaintInput()
				} else {
					repaintOption(focus)
				}
			}
		case len(k) >= 3 && k[0] == 27 && k[1] == '[' && k[2] == 'Z': // Shift+Tab
			if focus == fTags {
				focus = fName
				repaintInput()
			}
		case len(k) == 1 && (k[0] == 3 || (k[0] == 27 && n == 1)): // Ctrl-C / Esc
			clearWidget()
			return "", nil, -1, false
		case focus >= 0 && len(k) == 1 && (k[0] == '\r' || k[0] == '\n'):
			clearWidget()
			return "", nil, focus, true
		case focus == fName:
			switch {
			case len(k) == 1 && (k[0] == ' ' || k[0] == '\t' || k[0] == '\r' || k[0] == '\n'):
				focus = fTags
				repaintInput()
			case len(k) == 1 && (k[0] == 127 || k[0] == 8):
				if len(nm) > 0 {
					nm = nm[:len(nm)-1]
					repaintInput()
				}
			case len(k) == 1 && k[0] >= 32 && k[0] < 127:
				nm = append(nm, rune(k[0]))
				repaintInput()
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
				clearWidget()
				return name, strings.Fields(string(tg)), -1, true
			case len(k) == 1 && (k[0] == 127 || k[0] == 8):
				if len(tg) > 0 {
					tg = tg[:len(tg)-1]
				} else {
					focus = fName
				}
				repaintInput()
			case len(k) == 1 && k[0] >= 32 && k[0] < 127:
				tg = append(tg, rune(k[0]))
				repaintInput()
			}
		}
	}
}

// numberedOrCreate is the non-TTY fallback for selectOrCreate.
func numberedOrCreate(defaultName string, options []string) (string, []string, int, bool) {
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
