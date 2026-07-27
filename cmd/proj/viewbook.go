package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/daemon"
	"github.com/tieo/proj/internal/projects"
	"github.com/tieo/proj/internal/viewbook"
)

var (
	viewbookPort int
	viewbookDir  string
)

var viewbookCmd = &cobra.Command{
	Use:   "viewbook [project]",
	Short: "serve a project's model of its own views, wired to its session",
	Long: `Serve a project's viewbook: every view of its app, what each has to do, the
states it can be in, and how each renders today.

The model is plain files in the project's working tree, so whoever is working in
that session edits exactly what the browser shows, and the page updates itself
the moment one of those files changes. A change made in the browser is sent into
that project's session as a message, because the session is where the
conversation lives and a second inbox would only split it.

The model lives in docs/model by default; --dir points elsewhere. With no
project named, the project holding the working directory is used.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runViewbook,
}

func init() {
	viewbookCmd.Flags().IntVar(&viewbookPort, "port", 8099, "port to serve on")
	viewbookCmd.Flags().StringVar(&viewbookDir, "dir", "docs/model", "model directory, relative to the project")
	rootCmd.AddCommand(viewbookCmd)
}

func runViewbook(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	query := ""
	if len(args) == 1 {
		query = args[0]
	} else {
		here, err := os.Getwd()
		if err != nil {
			return err
		}
		query = filepath.Base(here)
	}
	p, err := projects.Resolve(cfg.BaseDir, query)
	if err != nil {
		return err
	}

	root := filepath.Join(p.Dir, viewbookDir)
	if _, err := os.Stat(filepath.Join(root, "model.json")); err != nil {
		return fmt.Errorf("no model.json in %s; point --dir at the model directory", root)
	}

	session := projects.SessionName(p.Name, p.Tags)
	server := &viewbook.Server{
		Root: root,
		// Everything the browser changes is spoken into the session, so the
		// project has one conversation rather than a page and a chat that each
		// know half of it.
		Say: func(message string) error {
			pane := paneForSession(session)
			if pane == "" {
				return fmt.Errorf("%s has no running session", p.Name)
			}
			return daemon.SendPrompt(daemon.DefaultConfig(), pane, message)
		},
	}

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", viewbookPort))
	if err != nil {
		return fmt.Errorf("listen on %d: %w", viewbookPort, err)
	}

	stop := make(chan struct{})
	go server.Watch(stop)

	http := &http.Server{Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := http.Serve(listener); err != nil {
			fmt.Fprintln(os.Stderr, "viewbook:", err)
		}
	}()

	fmt.Printf("%s viewbook on http://127.0.0.1:%d\n", p.Name, viewbookPort)
	fmt.Printf("model  %s\n", root)
	if paneForSession(session) == "" {
		fmt.Printf("note   %s has no running session, so changes have nowhere to go yet\n", p.Name)
	} else {
		fmt.Printf("speaks into session %s\n", session)
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	close(stop)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return http.Shutdown(ctx)
}
