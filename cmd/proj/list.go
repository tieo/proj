package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tieo/proj/internal/config"
	"github.com/tieo/proj/internal/projects"
)

const (
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiGray   = "\033[90m"
	ansiDim    = "\033[2m"
	ansiReset  = "\033[0m"
)

var listCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "list projects with session status",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		all := projects.All(cfg.BaseDir)
		sort.SliceStable(all, func(i, j int) bool {
			ai, aj := all[i].SessionTS, all[j].SessionTS
			if (ai > 0) != (aj > 0) {
				return ai > 0 // active first
			}
			if ai != aj {
				return ai > aj
			}
			return all[i].DirMTime > all[j].DirMTime
		})
		now := time.Now().Unix()
		for _, p := range all {
			color, dot, ts := ansiGray, "○", p.DirMTime
			if p.SessionTS > 0 {
				color, dot, ts = ansiGreen, "●", p.SessionTS
			}
			fmt.Printf("  %s%s%s %-22s %-15s %s%s%s\n",
				color, dot, ansiReset,
				p.Name, p.Lang,
				ansiDim, projects.Reltime(ts, now), ansiReset)
		}
		home := os.Getenv("HOME")
		for _, s := range projects.OrphanSessions(cfg.BaseDir) {
			path := s.Path
			if home != "" {
				path = strings.Replace(path, home, "~", 1)
			}
			fmt.Printf("  %s●%s %-22s %-15s %s%s%s\n",
				ansiYellow, ansiReset,
				s.Name, path,
				ansiDim, projects.Reltime(s.Activity, now), ansiReset)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(listCmd)
}
