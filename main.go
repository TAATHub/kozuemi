package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	maxCount := flag.Int("max", 5000, "maximum number of commits to load into the graph (0 = unlimited)")
	cwdFile := flag.String("cwd-file", "", "write the active worktree path to this file on exit (for a cd wrapper)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: gitscope [flags] [repo-dir]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	dir := "."
	if flag.NArg() > 0 {
		dir = flag.Arg(0)
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	top, err := Repo{Dir: dir}.run("rev-parse", "--show-toplevel")
	if err != nil {
		fmt.Fprintln(os.Stderr, "gitscope:", firstLine(err.Error()))
		os.Exit(1)
	}

	applyTheme()
	m := newModel(firstLine(top), *maxCount)
	final, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithReportFocus()).Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *cwdFile != "" {
		if fm, ok := final.(*Model); ok {
			_ = os.WriteFile(*cwdFile, []byte(fm.repo.Dir), 0o644)
		}
	}
}
