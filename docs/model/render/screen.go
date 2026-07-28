package main

import (
	"html"
	"strconv"
	"strings"
)

// A cell is one character on the screen with the attributes in force when it
// was written.
type cell struct {
	char      rune
	color     int // ANSI foreground code, 0 for the terminal's own
	bold      bool
	dim       bool
	selected  bool // the selector's full-width bar
	hasColor  bool
	underline bool
}

type line struct{ cells []cell }

func (c cell) html(th theme) string {
	text := html.EscapeString(string(c.char))
	var classes []string
	var styles []string
	if c.hasColor {
		if hex, ok := th.palette[c.color]; ok {
			styles = append(styles, "color:"+hex)
		}
	}
	if c.dim {
		classes = append(classes, "dim")
	}
	if c.bold {
		classes = append(classes, "bold")
	}
	if c.selected {
		classes = append(classes, "sel")
	}
	if c.underline {
		styles = append(styles, "text-decoration:underline")
	}
	if len(classes) == 0 && len(styles) == 0 {
		return text
	}
	var b strings.Builder
	b.WriteString("<span")
	if len(classes) > 0 {
		b.WriteString(` class="` + strings.Join(classes, " ") + `"`)
	}
	if len(styles) > 0 {
		b.WriteString(` style="` + strings.Join(styles, ";") + `"`)
	}
	b.WriteString(">" + text + "</span>")
	return b.String()
}

// replay turns a program's raw output into the screen a terminal would be
// showing after it. It carries only what proj emits: colours, dim, bold, the
// selector's background bar, erase-to-end-of-line, and the cursor moves a
// redrawn list makes.
func replay(out string, cols int) []line {
	scr := &screen{cols: cols}
	runes := []rune(out)
	var cur cell
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == '\x1b' && i+1 < len(runes) && runes[i+1] == '[':
			seq, params, final := readCSI(runes, i+2)
			i = seq
			switch final {
			case 'm':
				applySGR(&cur, params)
			case 'K':
				scr.eraseToEnd()
			case 'A':
				scr.move(-atoiDefault(params, 1), 0)
			case 'B':
				scr.move(atoiDefault(params, 1), 0)
			case 'C':
				scr.move(0, atoiDefault(params, 1))
			case 'D':
				scr.move(0, -atoiDefault(params, 1))
			case 'H':
				scr.row, scr.col = 0, 0
			case 'J':
				scr.clear()
			}
		case r == '\x1b' && i+1 < len(runes) && runes[i+1] == ']':
			// An OSC (a hyperlink, a title) ends at BEL or ST and shows nothing
			// of itself; its label text keeps flowing after it.
			for i+1 < len(runes) && runes[i] != '\a' && !(runes[i] == '\x1b' && runes[i+1] == '\\') {
				i++
			}
			i++
		case r == '\n':
			scr.newline()
		case r == '\r':
			scr.col = 0
		case r == '\t':
			for scr.col%8 != 0 || scr.col == 0 {
				scr.put(cell{char: ' '})
				if scr.col%8 == 0 {
					break
				}
			}
		case r < ' ':
			// Bell and friends draw nothing.
		default:
			c := cur
			c.char = r
			scr.put(c)
		}
	}
	return scr.lines()
}

type screen struct {
	cols int
	rows [][]cell
	row  int
	col  int
}

func (s *screen) ensure(row, col int) {
	for len(s.rows) <= row {
		s.rows = append(s.rows, nil)
	}
	for len(s.rows[row]) <= col {
		s.rows[row] = append(s.rows[row], cell{char: ' '})
	}
}

func (s *screen) put(c cell) {
	if s.cols > 0 && s.col >= s.cols {
		s.newline()
	}
	s.ensure(s.row, s.col)
	s.rows[s.row][s.col] = c
	s.col++
}

func (s *screen) newline() {
	s.row++
	s.col = 0
	s.ensure(s.row, 0)
}

func (s *screen) move(rows, cols int) {
	s.row += rows
	s.col += cols
	if s.row < 0 {
		s.row = 0
	}
	if s.col < 0 {
		s.col = 0
	}
	s.ensure(s.row, s.col)
}

func (s *screen) eraseToEnd() {
	if s.row < len(s.rows) && s.col < len(s.rows[s.row]) {
		s.rows[s.row] = s.rows[s.row][:s.col]
	}
}

func (s *screen) clear() {
	s.rows = nil
	s.row, s.col = 0, 0
}

// lines returns the screen with its trailing blank rows dropped and each row
// trimmed of the padding cells nothing was written into.
func (s *screen) lines() []line {
	out := make([]line, 0, len(s.rows))
	for _, row := range s.rows {
		end := len(row)
		for end > 0 && row[end-1].char == ' ' && !row[end-1].selected {
			end--
		}
		out = append(out, line{cells: row[:end]})
	}
	for len(out) > 0 && len(out[len(out)-1].cells) == 0 {
		out = out[:len(out)-1]
	}
	return out
}

func readCSI(runes []rune, start int) (end int, params string, final rune) {
	i := start
	for i < len(runes) && (runes[i] == ';' || runes[i] == '?' || (runes[i] >= '0' && runes[i] <= '9')) {
		i++
	}
	params = string(runes[start:i])
	if i < len(runes) {
		final = runes[i]
	}
	return i, params, final
}

func applySGR(c *cell, params string) {
	if params == "" {
		params = "0"
	}
	fields := strings.Split(params, ";")
	for i := 0; i < len(fields); i++ {
		n, err := strconv.Atoi(fields[i])
		if err != nil {
			continue
		}
		switch {
		case n == 0:
			*c = cell{}
		case n == 1:
			c.bold = true
		case n == 2:
			c.dim = true
		case n == 4:
			c.underline = true
		case n == 22:
			c.bold, c.dim = false, false
		case n == 39:
			c.hasColor = false
		case n >= 30 && n <= 37, n >= 90 && n <= 97:
			c.color, c.hasColor = n, true
		case n == 38 || n == 48:
			// 38;5;N and 48;5;N: only the selector's background bar is used,
			// and it is the one thing that has to survive.
			if i+2 < len(fields) && fields[i+1] == "5" {
				if n == 48 {
					c.selected = true
				} else if idx, err := strconv.Atoi(fields[i+2]); err == nil {
					c.color, c.hasColor = ansi256(idx), true
				}
				i += 2
			}
		case n == 49:
			c.selected = false
		}
	}
}

// ansi256 folds a 256-colour index onto the sixteen the themes carry, which is
// all proj uses beyond its own greys.
func ansi256(idx int) int {
	switch {
	case idx < 8:
		return 30 + idx
	case idx < 16:
		return 90 + (idx - 8)
	case idx >= 232:
		return 90
	default:
		return 37
	}
}

func atoiDefault(params string, fallback int) int {
	if params == "" {
		return fallback
	}
	n, err := strconv.Atoi(strings.Split(params, ";")[0])
	if err != nil {
		return fallback
	}
	return n
}

// plain renders a screen as the text it shows, for checking a scene without
// looking at its picture.
func plain(screen []line) string {
	var b strings.Builder
	for _, l := range screen {
		for _, c := range l.cells {
			b.WriteRune(c.char)
		}
		b.WriteString("\n")
	}
	return b.String()
}
