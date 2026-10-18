package main

import (
	"context"
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
	input := flag.String("input", "", "forecast JSON file to open (default: <project>/output/forecast.json)")
	project := flag.String("home", "", "WeatherNext project folder (or set WEATHERNEXT_HOME)")
	flag.Parse()
	if *project != "" {
		_ = os.Setenv("WEATHERNEXT_HOME", *project)
	}
	root, rootErr := backend.FindProjectRoot()
	cwd, _ := os.Getwd()
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
	if *printMode {
		if readErr != nil {
			fatal(fmt.Errorf("open %s: %w", path, readErr))
		}
		if err := tui.Print(raw, os.Stdout); err != nil {
			fatal(err)
		}
		return
	}
	var client *backend.Client
	if rootErr == nil {
		var err error
		client, err = backend.Start(context.Background(), root)
		if err != nil {
			fmt.Fprintln(os.Stderr, "WeatherNext worker unavailable:", err)
		}
	}
	if client != nil {
		defer client.Close()
	}
	if readErr != nil && *input != "" {
		fmt.Fprintf(os.Stderr, "Cached forecast %s could not be opened: %v\n", path, readErr)
	}
	openPath := path
	if readErr != nil && *input == "" {
		// Let the worker open its durable latest forecast from local storage if
		// the conventional JSON file is absent.
		openPath = ""
	}
	model := tui.NewWithPath(raw, client, openPath)
	p := tea.NewProgram(&model)
	if _, err := p.Run(); err != nil {
		fatal(err)
	}
}

func fatal(err error) { fmt.Fprintln(os.Stderr, "weathernext:", err); os.Exit(1) }
