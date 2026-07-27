package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/daemon"
	"github.com/tieo/proj/internal/projects"
	"github.com/tieo/proj/internal/tmux"
	"github.com/tieo/proj/internal/viewbook"
)

var (
	viewbookListen string
	viewbookDir    string
)

var viewbookCmd = &cobra.Command{
	Use:   "viewbook [project]",
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
	rootCmd.AddCommand(viewbookCmd)
}

// sayInto delivers a message to the project's session, which is where its
// conversation happens. A project with no session running keeps its change in
// the file and says so rather than pretending it was passed on.
func sayInto(p projects.Project) func(string) error {
	session := projects.SessionName(p.Name, p.Tags)
	return func(message string) error {
		pane := paneForSession(session)
		if pane == "" {
			return fmt.Errorf("%s has no running session", p.Name)
		}
		return daemon.SendPrompt(daemon.DefaultConfig(), pane, message)
	}
}

// readSession returns what the project's session currently shows, so a page can
// display the reply to what it just said instead of leaving someone guessing.
func readSession(p projects.Project) func() string {
	session := projects.SessionName(p.Name, p.Tags)
	return func() string {
		pane := paneForSession(session)
		if pane == "" {
			return ""
		}
		return tmux.CapturePane(pane, 200)
	}
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
		server := &viewbook.Server{Root: root, Say: sayInto(p), Session: readSession(p)}
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
	http := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := http.Serve(listener); err != nil {
			fmt.Fprintln(os.Stderr, "viewbook:", err)
		}
	}()

	fmt.Printf("viewbook on http://%s\n", viewbookListen)
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
