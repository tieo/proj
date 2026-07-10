package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/daemon"
	"github.com/tieo/proj/internal/projects"
)

const (
	ansiGreen  = "\033[32m"
	ansiRed    = "\033[31m"
	ansiYellow = "\033[33m"
	ansiGray   = "\033[90m"
	ansiDim    = "\033[2m"
	ansiReset  = "\033[0m"
)

type listRow struct {
	// indicator is the left-hand status symbol, always 2 terminal columns wide:
	//   📌  (pinned + alive)
	//   ●·  (alive; colored dot + space)
	//   ○·  (dead; grey circle + space)
	indicator string
	name      string
	tags      string // space-joined tags, or abbreviated path for orphans
	model     string // empty when not detected
	ts        int64
	note      string // plain-text description of a non-normal state
	noteColor string // ANSI color for the note, empty = dim
}

var (
	listAll  bool
	listTagF string
)

var listCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "list projects with session status",
	RunE:    runList,
}

func runList(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	unrCfg := daemonConfig()
	managed, err := daemon.LoadManagedState(unrCfg.StatePath)
	if err != nil {
		return fmt.Errorf("managed state unreadable (pins are only stored there): %w", err)
	}

	// Scan panes for label (banner/error/selector state) and, as a fallback, RC
	// status per session. Model is read from JSONL session files instead; more
	// reliable.
	scan := daemon.ScanPanes(cfg.Claude.Home, unrCfg.Capture)
	labelBySession := make(map[string]string, len(scan))
	rcBySession := make(map[string]string, len(scan))
	for _, s := range scan {
		n := s.Pane.Session
		label := s.Label()
		existing := labelBySession[n]
		if existing == "" || (label != "idle" && existing == "idle") {
			labelBySession[n] = label
		}
		// Merge RC status across panes: active wins over offline wins over "".
		rc := s.RC
		existRC := rcBySession[n]
		if rc == "active" || (rc == "offline" && existRC == "") {
			rcBySession[n] = rc
		}
	}

	// Authoritative RC status from Claude's own per-session bridge files (local,
	// non-rotating). nil when unreadable, in which case the pane /rc hyperlink
	// (rcBySession) is the fallback.
	rcBridges := daemon.RCBridges(cfg.Claude.Home)
	host, _ := os.Hostname()

	all := projects.All(cfg.BaseDir)
	if listTagF != "" {
		filtered := all[:0]
		for _, p := range all {
			if hasTag(p.Tags, listTagF) {
				filtered = append(filtered, p)
			}
		}
		all = filtered
	}
	sort.SliceStable(all, func(i, j int) bool {
		mi := managed[projects.SessionName(all[i].Name, all[i].Tags)]
		mj := managed[projects.SessionName(all[j].Name, all[j].Tags)]
		if mi.Pinned != mj.Pinned {
			return mi.Pinned
		}
		ai, aj := all[i].SessionTS, all[j].SessionTS
		if (ai > 0) != (aj > 0) {
			return ai > 0
		}
		if ai != aj {
			return ai > aj
		}
		return all[i].DirMTime > all[j].DirMTime
	})

	var rows []listRow
	now := time.Now().Unix()

	maxAge := cfg.List.MaxAgeDays
	if listAll {
		maxAge = 0
	}
	cutoff := int64(0)
	if maxAge > 0 {
		cutoff = now - int64(maxAge)*86400
	}
	hidden := 0

	for _, p := range all {
		sessName := projects.SessionName(p.Name, p.Tags)
		ms, tracked := managed[sessName]
		label := labelBySession[sessName]
		alive := p.SessionTS > 0
		rc := rcBySession[sessName]
		// Remote Control is a Claude Code feature; other tools get no RC note.
		if daemon.ToolName(p.Tool) != config.DefaultTool {
			rc = ""
		} else if alive && rcBridges != nil {
			if rcBridges[daemon.RCName(sessName, host)] {
				rc = "active"
			} else {
				rc = "offline"
			}
		}

		// Hide inactive projects older than the cutoff (active and pinned always shown).
		if cutoff > 0 && !alive && !ms.Pinned && p.DirMTime < cutoff {
			hidden++
			continue
		}

		rows = append(rows, listRow{
			indicator: buildIndicator(alive, ms.Pinned, label, rc),
			name:      p.Name,
			tags:      strings.Join(p.Tags, " "),
			model:     modelLabel(p, cfg.Claude.Home),
			ts:        sessionTS(p, alive),
			note:      buildNote(label, rc, ms, tracked, alive, unrCfg.KeepAlive),
			noteColor: noteColor(label, rc, alive),
		})
	}

	if listTagF == "" {
		home := os.Getenv("HOME")
		for _, s := range projects.OrphanSessions(cfg.BaseDir) {
			ms, tracked := managed[s.Name]
			label := labelBySession[s.Name]
			rc := rcBySession[s.Name] // orphans are always alive
			if rcBridges != nil {
				if rcBridges[daemon.RCName(s.Name, host)] {
					rc = "active"
				} else {
					rc = "offline"
				}
			}
			path := shortPath(strings.Replace(s.Path, home, "~", 1), 48)
			rows = append(rows, listRow{
				indicator: buildIndicator(true, ms.Pinned, label, rc),
				name:      s.Name,
				tags:      path,
				model:     "", // orphan: no known project dir for JSONL lookup
				ts:        s.Activity,
				note:      buildNote(label, rc, ms, tracked, true, unrCfg.KeepAlive),
				noteColor: noteColor(label, rc, true),
			})
		}
	}

	// Adaptive column widths: max content width + 2, with minimums.
	nameW, tagsW, modelW := 8, 5, 0
	for _, r := range rows {
		if len(r.name) > nameW {
			nameW = len(r.name)
		}
		if len(r.tags) > tagsW {
			tagsW = len(r.tags)
		}
		if len(r.model) > modelW {
			modelW = len(r.model)
		}
	}
	nameW += 2
	tagsW += 2
	if modelW > 0 {
		modelW += 2
	}

	for _, r := range rows {
		line := fmt.Sprintf("  %s %-*s %-*s", r.indicator, nameW, r.name, tagsW, r.tags)
		if modelW > 0 {
			line += fmt.Sprintf("%-*s", modelW, r.model)
		}
		line += fmt.Sprintf("%s%s%s", ansiDim, projects.Reltime(r.ts, now), ansiReset)
		if r.note != "" {
			nc := r.noteColor
			if nc == "" {
				nc = ansiDim
			}
			line += "  " + nc + r.note + ansiReset
		}
		fmt.Println(line)
	}
	if hidden > 0 {
		fmt.Printf("%s  + %d older projects hidden (--all to show)%s\n", ansiDim, hidden, ansiReset)
	}
	return nil
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

func shortPath(path string, max int) string {
	if max <= 0 || len(path) <= max {
		return path
	}
	clean := filepath.Clean(path)
	parent := filepath.Base(filepath.Dir(clean))
	base := filepath.Base(clean)
	out := filepath.Join("...", parent, base)
	if len(out) <= max {
		return out
	}
	if len(base)+4 <= max {
		return ".../" + base
	}
	if max <= 3 {
		return path[:max]
	}
	return "..." + base[len(base)-(max-3):]
}

// buildIndicator returns a 2-terminal-column-wide status symbol.
//
//	📌   pinned (alive or dead, emoji, 2 cols)
//	● ·  alive; colored dot + space (1+1 cols)
//	○ ·  dead ; grey circle + space (1+1 cols)
//
// modelLabel fills the model column with the detected model when available.
func modelLabel(p projects.Project, claudeHome string) string {
	tool := daemon.ToolName(p.Tool)
	switch tool {
	case config.DefaultTool:
		if model := daemon.ModelFromDir(claudeHome, p.Dir); model != "" {
			return model
		}
	case "codex":
		if model := daemon.CodexModelFromDir(p.Dir); model != "" {
			return model
		}
		return tool
	case "agy":
		if model := daemon.AgyModelFromDir(p.Dir); model != "" {
			return model
		}
		return tool
	}
	return ""
}

func buildIndicator(alive, pinned bool, label, rc string) string {
	if pinned {
		return "📌"
	}
	if !alive {
		return ansiGray + "○" + ansiReset + " "
	}
	switch label {
	case "error", "banner", "banner + selector":
		return ansiRed + "●" + ansiReset + " "
	case "selector":
		return ansiYellow + "●" + ansiReset + " "
	}
	if rc == "offline" {
		return ansiYellow + "●" + ansiReset + " "
	}
	return ansiGreen + "●" + ansiReset + " " // "active" or "" (no zone yet — starting up)
}

func buildNote(label, rc string, ms daemon.ManagedSession, tracked, alive, globalKeepAlive bool) string {
	// Only the daemon's tracked sessions get recreated. globalKeepAlive on its
	// own must not paint every dead project as "restarting".
	if !alive && tracked && (ms.Pinned || ms.KeepAlive || globalKeepAlive) && !ms.ExitedCleanly {
		return "restarting"
	}
	switch label {
	case "banner", "banner + selector":
		return "out of tokens"
	case "error":
		return "API error"
	case "selector":
		return "waiting for input"
	}
	if rc == "offline" {
		return "RC offline"
	}
	return ""
}

func noteColor(label, rc string, alive bool) string {
	if !alive {
		return ansiDim
	}
	switch label {
	case "error", "banner", "banner + selector":
		return ansiRed
	case "selector":
		return ansiYellow
	}
	if rc == "offline" {
		return ansiYellow
	}
	return ansiDim
}

func sessionTS(p projects.Project, alive bool) int64 {
	if alive {
		return p.SessionTS
	}
	return p.DirMTime
}

func init() {
	listCmd.Flags().StringVar(&listTagF, "tag", "", "only show projects with this tag")
	rootCmd.AddCommand(listCmd)
}
