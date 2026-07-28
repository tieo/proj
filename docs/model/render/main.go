// Command render draws every view of proj into docs/model/img.
//
// proj is a terminal program, so a render is a picture of a terminal: the real
// binary runs against a fabricated home (its own config, projects, tmux and
// claude state), its output is replayed through a small terminal model into
// HTML, and a headless browser photographs that. Nothing here reads or touches
// the machine's own sessions, so the pictures are the same on any machine.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// A shape is the terminal the picture is taken in. Both are real windows
// someone runs proj in: a wide one, and a half-screen column beside an editor.
type shape struct {
	name string
	cols int
	rows int
}

var shapes = []shape{
	{"wide", 104, 26},
	{"narrow", 52, 34},
}

// A theme is the terminal's own palette, which proj inherits: it never sets a
// background, and its greys and greens have to stay readable on both.
type theme struct {
	name       string
	background string
	foreground string
	dim        string
	palette    map[int]string
	selection  string
}

var themes = []theme{
	{
		name: "dark", background: "#16181d", foreground: "#c8d0e0", dim: "#7b8496",
		selection: "#2a2e39",
		palette: map[int]string{
			30: "#3b4048", 31: "#e06c75", 32: "#7fd88f", 33: "#e5c07b", 34: "#61afef",
			35: "#c678dd", 36: "#56b6c2", 37: "#c8d0e0", 90: "#6b7280",
		},
	},
	{
		name: "light", background: "#fbfbfa", foreground: "#24292f", dim: "#8a919b",
		selection: "#e6e8eb",
		palette: map[int]string{
			30: "#24292f", 31: "#b3261e", 32: "#2c7a45", 33: "#8a6100", 34: "#0a5cc4",
			35: "#8250df", 36: "#136f75", 37: "#24292f", 90: "#8a919b",
		},
	},
}

// A scene is one picture: a command run against one fixture, named for the
// view or state it stands for.
type scene struct {
	file    string
	fixture string
	args    []string
	tty     bool
	stdin   string
}

var scenes = []scene{
	{file: "list", fixture: "fleet", args: []string{"list"}},
	{file: "list-empty", fixture: "no-projects", args: []string{"list"}},
	{file: "list-failed", fixture: "broken-config", args: []string{"list"}},

	{file: "sessions", fixture: "fleet", args: []string{"sessions"}, tty: true, stdin: "q"},
	{file: "sessions-empty", fixture: "no-sessions", args: []string{"sessions"}},
	{file: "sessions-failed", fixture: "broken-config", args: []string{"sessions"}},

	{file: "doner", fixture: "fleet", args: []string{"doner"}},
	{file: "doner-empty", fixture: "no-doner", args: []string{"doner"}},
	{file: "doner-failed", fixture: "broken-config", args: []string{"doner"}},

	{file: "switch", fixture: "fleet", args: []string{"switch", "nasx", "codex", "--dry-run"}},
	{file: "switch-empty", fixture: "fleet", args: []string{"switch", "claudemd", "codex", "--dry-run"}},
	{file: "switch-failed", fixture: "fleet", args: []string{"switch", "nasx", "cursor", "--dry-run"}},

	{file: "model", fixture: "fleet", args: []string{"model", "opus"}},
	{file: "model-empty", fixture: "no-projects", args: []string{"model", "opus"}},
	{file: "model-failed", fixture: "broken-config", args: []string{"model", "opus"}},
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "render:", err)
		os.Exit(1)
	}
}

func run() error {
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	outDir := filepath.Join(root, "docs", "model", "img")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "proj-render-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	binary := filepath.Join(work, "proj")
	fmt.Println("building proj")
	build := exec.Command("go", "build", "-o", binary, "./cmd/proj")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("build proj: %v\n%s", err, out)
	}

	fixtures := map[string]string{}
	written := 0
	for _, sc := range scenes {
		home, ok := fixtures[sc.fixture]
		if !ok {
			home = filepath.Join(work, "fixture-"+sc.fixture)
			if err := buildFixture(sc.fixture, home); err != nil {
				return fmt.Errorf("fixture %s: %w", sc.fixture, err)
			}
			fixtures[sc.fixture] = home
		}
		for _, sh := range shapes {
			output, err := capture(binary, home, sc, sh)
			if err != nil {
				return fmt.Errorf("scene %s (%s): %w", sc.file, sh.name, err)
			}
			screen := replay(output, sh.cols)
			for _, th := range themes {
				page := filepath.Join(work, fmt.Sprintf("%s-%s-%s.html", sc.file, sh.name, th.name))
				if err := os.WriteFile(page, []byte(document(screen, sh, th)), 0o644); err != nil {
					return err
				}
				png := filepath.Join(outDir, fmt.Sprintf("%s-%s-%s.png", sc.file, sh.name, th.name))
				if err := shoot(page, png, sh); err != nil {
					return fmt.Errorf("shoot %s: %w", filepath.Base(png), err)
				}
				written++
			}
		}
		fmt.Printf("  %s\n", sc.file)
	}
	fmt.Printf("%d renders in docs/model/img\n", written)
	return nil
}

// capture runs one scene and returns everything it wrote, escape sequences
// included. An interactive scene needs a terminal on stdin before it draws its
// moving cursor at all, so it runs under script(1), which gives it one.
func capture(binary, home string, sc scene, sh shape) (string, error) {
	env := []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_STATE_HOME=" + filepath.Join(home, ".local", "state"),
		"PATH=" + filepath.Join(home, "bin") + ":" + os.Getenv("PATH"),
		"TERM=xterm-256color",
		"COLUMNS=" + strconv.Itoa(sh.cols),
		"LINES=" + strconv.Itoa(sh.rows),
		// The stty stub reads the shape from here, which is how proj measures a
		// terminal that only exists as a picture.
		"PROJ_RENDER_SIZE=" + fmt.Sprintf("%d %d", sh.rows, sh.cols),
	}

	// A view that waits for a key would wait forever here, so every scene is
	// given the time one command takes and no more.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	if sc.tty {
		line := shellQuote(binary) + " " + strings.Join(quoteAll(sc.args), " ")
		cmd = exec.CommandContext(ctx, "script", "-qec", line, "/dev/null")
	} else {
		cmd = exec.CommandContext(ctx, binary, sc.args...)
	}
	cmd.Env = env
	cmd.Dir = home
	if sc.stdin != "" {
		cmd.Stdin = strings.NewReader(sc.stdin)
	}
	// A scene that ends in an error is a scene about that error, so the exit
	// status is part of the picture rather than a failure to take it.
	out, _ := cmd.CombinedOutput()
	return string(out), nil
}

func quoteAll(args []string) []string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuote(a)
	}
	return quoted
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// document wraps a replayed screen in a page sized to the shape, so the
// browser photographs a terminal rather than a web page that contains one.
func document(screen []line, sh shape, th theme) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<!doctype html><meta charset="utf-8"><style>
html,body{margin:0;padding:0;background:%s;}
body{padding:18px 20px;}
pre{margin:0;font-family:"JetBrains Mono","DejaVu Sans Mono",monospace;font-size:14px;line-height:1.45;
    color:%s;white-space:pre;letter-spacing:0;}
.dim{color:%s;}
.bold{font-weight:600;}
.sel{background:%s;}
</style><pre>`, th.background, th.foreground, th.dim, th.selection)
	for _, l := range screen {
		for _, c := range l.cells {
			b.WriteString(c.html(th))
		}
		b.WriteString("\n")
	}
	b.WriteString("</pre>")
	return b.String()
}

func shoot(page, png string, sh shape) error {
	// 8.42px per column and 20.3px per row is the JetBrains Mono cell at 14px,
	// plus the page padding; the window is sized from the shape so the picture
	// is the terminal and nothing else.
	width := int(float64(sh.cols)*8.42) + 44
	height := int(float64(sh.rows)*20.3) + 40
	cmd := exec.Command("chromium",
		"--headless=new",
		"--disable-gpu",
		"--no-sandbox",
		"--hide-scrollbars",
		"--force-device-scale-factor=2",
		"--default-background-color=00000000",
		fmt.Sprintf("--window-size=%d,%d", width, height),
		"--screenshot="+png,
		"file://"+page,
	)
	cmd.Env = append(os.Environ(), "HOME="+filepath.Dir(page))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v\n%s", err, out)
	}
	return nil
}
