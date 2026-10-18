package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	tea "charm.land/bubbletea/v2"
	"weathernext/internal/backend"
	"weathernext/internal/tui"
)

func main() {
	printMode := flag.Bool("print", false, "print a static wttr.in-style summary and exit")
	input := flag.String("input", "", "forecast JSON file to open (default: <home>/output/forecast.json)")
	project := flag.String("home", "", "folder for local forecast files and Google login state (or set WEATHERNEXT_HOME)")
	flag.Parse()
	if *project != "" {
		_ = os.Setenv("WEATHERNEXT_HOME", *project)
	}
	root, rootErr := backend.FindHome()
	cwd, _ := os.Getwd()
	if *printMode {
		path := *input
		if path == "" {
			base := cwd
			if rootErr == nil {
				base = root
			}
			path = filepath.Join(base, "output", "forecast.json")
		} else if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			fatal(fmt.Errorf("open %s: %w", path, readErr))
		}
		if err := tui.Print(raw, os.Stdout); err != nil {
			fatal(err)
		}
		return
	}
	var caller tui.Caller
	if rootErr != nil {
		fmt.Fprintln(os.Stderr, "WeatherNext local folder unavailable:", rootErr)
	} else {
		client, err := backend.StartNative(root)
		if err != nil {
			fmt.Fprintln(os.Stderr, "WeatherNext local backend unavailable:", err)
		} else if client != nil {
			defer client.Close()
			caller = client
		} else {
			fmt.Fprintln(os.Stderr, "WeatherNext local backend unavailable: no client was returned")
		}
	}
	openPath := ""
	if *input != "" {
		openPath = *input
		if !filepath.IsAbs(openPath) {
			openPath = filepath.Join(cwd, openPath)
		}
	}
	model := tui.NewWithPath(nil, caller, openPath)
	p := tea.NewProgram(&model)
	if _, err := p.Run(); err != nil {
		fatal(err)
	}
}

func fatal(err error) { fmt.Fprintln(os.Stderr, "weathernext:", err); os.Exit(1) }
