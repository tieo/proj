package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/daemon"
	"github.com/tieo/proj/internal/projects"
	"github.com/tieo/proj/internal/tmux"
	"github.com/tieo/viewbook"
)

var (
	viewbookListen string
	viewbookDir    string
	viewbookKey    string
)

var viewbookCmd = &cobra.Command{
	// Named "book", not "viewbook": a subcommand shadows a project of the same
	// name, and viewbook is a project here like any other.
	Use:   "book [project]",
	Short: "serve a project's model of its own views, wired to its session",
	Long: `Serve a viewbook: every view of an app, what each has to do, the states it
can be in, and how each renders today.

Named a project, it serves that one at the root. Named none, it serves every
project that has a model, each under its own path, with a list of them at the
root - one address per project, so a link to a view stays a link to that view.

The model is plain files in the project's working tree, so whoever is working in
that session edits exactly what the browser shows, and the page updates itself
the moment one of those files changes. A change made in the browser is sent into
that project's session as a message, because the session is where the
conversation lives and a second inbox would only split it.

The model lives in docs/model by default; --dir points elsewhere.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runViewbook,
}

func init() {
	viewbookCmd.Flags().StringVar(&viewbookListen, "listen", "127.0.0.1:8099", "address to serve on")
	viewbookCmd.Flags().StringVar(&viewbookDir, "dir", "docs/model", "model directory, relative to the project")
	viewbookCmd.Flags().StringVar(&viewbookKey, "key-file", defaultKeyPath(),
		"file holding the key the browser must carry; empty serves to anyone who reaches the port")
	rootCmd.AddCommand(viewbookCmd)
}

// defaultKeyPath is where the key that opens the books is kept, next to the
// rest of proj's state and readable by its owner alone.
func defaultKeyPath() string { return projStateDir("viewbook.key") }

// sayInto delivers a message to the project's session, which is where its
// conversation happens. A project with no session running keeps its change in
// the file and says so rather than pretending it was passed on.
func sayInto(p projects.Project) func(string) error {
	return func(message string) error {
		pane := paneForProject(p)
		if pane == "" {
			return fmt.Errorf("%s has no running session", p.Name)
		}
		return daemon.SendPrompt(daemon.DefaultConfig(), pane, message)
	}
}

// paneForProject finds the project's session now rather than remembering what
// it was called when this server started. A session is renamed whenever its
// tags change - turning doner on and off does exactly that - and a book that
// held the old name told everyone the project had no session at all.
func paneForProject(p projects.Project) string {
	if pane := paneForSession(projects.SessionName(p.Name, p.Tags)); pane != "" {
		return pane
	}
	// Tags may have changed since; the name before the @ is the project.
	for _, pane := range tmux.ListPanes() {
		if name, _, _ := strings.Cut(pane.Session, "@"); name == projects.SessionName(p.Name, nil) {
			return pane.ID
		}
	}
	return ""
}

// readSession returns what the project's session has been saying, so a page can
// show the reply to what it just asked instead of leaving someone guessing.
//
// The pane's own furniture - the composer, the status line, the hint bar - is
// cut off the end: it is the same three lines on every read and says nothing
// about the conversation.
func readSession(p projects.Project) func() string {
	return func() string {
		pane := paneForProject(p)
		if pane == "" {
			return ""
		}
		return conversation(tmux.CapturePane(pane, 600))
	}
}

// wakeSession starts the project's session if it is not already running, the
// same way opening the project from a terminal does. Opening a book is asking
// to work on that project; waiting for someone to go and start a session first
// is a step nobody wanted.
func wakeSession(cfg config.Config, p projects.Project) func() error {
	return func() error {
		session := projects.SessionName(p.Name, p.Tags)
		switch existing := tmux.SessionForPath(p.Dir); existing {
		case session:
			return nil
		case "":
			spec, err := cfg.Tool(p.Tool)
			if err != nil {
				return err
			}
			line := daemon.LaunchCommand(spec, cfg.Claude.Home, p.Name, session, p.Dir)
			if _, err := tmux.NewSession(session, p.Dir, line); err != nil {
				return fmt.Errorf("start %s: %w", p.Name, err)
			}
			return nil
		default:
			// A session for this directory under an older name is still that
			// project's session; renaming is what open does too.
			return tmux.RenameSession(existing, session)
		}
	}
}

// restSession stops the project's session, which is the same decision as
// starting it, made the other way.
func restSession(p projects.Project) func() error {
	return func() error {
		existing := tmux.SessionForPath(p.Dir)
		if existing == "" {
			return nil
		}
		return tmux.KillSession(existing)
	}
}

// composerTop matches the rule the input box is drawn above, which is where the
// conversation ends and the TUI's own chrome begins.
var composerTop = regexp.MustCompile(`^[\s]*[─━]{10,}`)

func conversation(pane string) string {
	lines := strings.Split(strings.TrimRight(pane, "\n"), "\n")
	// The composer is drawn between two rules, with the status bar under the
	// lower one. Cutting at the first rule found from the end would leave the
	// input box in view, so the highest rule near the end is the one to cut at.
	cut := len(lines)
	for i := len(lines) - 1; i >= 0 && i > len(lines)-14; i-- {
		if composerTop.MatchString(lines[i]) {
			cut = i
		}
	}
	lines = lines[:cut]
	// Blank tails are what an idle pane is mostly made of.
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > 60 {
		lines = lines[len(lines)-60:]
	}
	return strings.Join(lines, "\n")
}

func runViewbook(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	var chosen []projects.Project
	if len(args) == 1 {
		p, err := projects.Resolve(cfg.BaseDir, args[0])
		if err != nil {
			return err
		}
		chosen = []projects.Project{p}
	} else {
		chosen = projects.All(cfg.BaseDir)
	}

	stop := make(chan struct{})
	var books []viewbook.Book
	for _, p := range chosen {
		root := filepath.Join(p.Dir, viewbookDir)
		if _, err := os.Stat(filepath.Join(root, "model.json")); err != nil {
			if len(args) == 1 {
				return fmt.Errorf("no model.json in %s; point --dir at the model directory", root)
			}
			continue // a project without a model simply has no book
		}
		server := &viewbook.Server{
			Root:    root,
			Say:     sayInto(p),
			Session: readSession(p),
			Wake:    wakeSession(cfg, p),
			Rest:    restSession(p),
		}
		go server.Watch(stop)
		books = append(books, viewbook.Book{
			Name:   strings.ToLower(p.Name),
			Title:  p.Name,
			Server: server,
		})
	}
	if len(books) == 0 {
		return fmt.Errorf("no project under %s has a %s/model.json", cfg.BaseDir, viewbookDir)
	}

	listener, err := net.Listen("tcp", viewbookListen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", viewbookListen, err)
	}

	var handler http.Handler
	if len(books) == 1 && len(args) == 1 {
		handler = books[0].Server.Handler("/")
	} else {
		handler = viewbook.Serve(books)
	}

	// What is typed here lands in a session that can run anything, so the book
	// is opened by whoever can read the key file and by nobody else.
	key := ""
	if viewbookKey != "" {
		key, err = viewbook.KeyAt(viewbookKey)
		if err != nil {
			return fmt.Errorf("read %s: %w", viewbookKey, err)
		}
	}
	http := &http.Server{Handler: viewbook.GuardFrom(key, viewbookKey, handler), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := http.Serve(listener); err != nil {
			fmt.Fprintln(os.Stderr, "viewbook:", err)
		}
	}()

	// The key goes in the address once. The browser keeps it from then on, and
	// the same key opens the book wherever it is published from here.
	opening := "http://" + viewbookListen + "/"
	if key != "" {
		opening += "?key=" + key
	}
	fmt.Printf("viewbook on %s\n", opening)
	for _, book := range books {
		where := "/"
		if len(books) > 1 || len(args) != 1 {
			where = "/" + book.Name + "/"
		}
		fmt.Printf("  %-24s %s\n", where, book.Server.Root)
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	close(stop)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return http.Shutdown(ctx)
}
