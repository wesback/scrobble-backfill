// Package cli defines the Rescrobble command tree and its command-independent
// profile selection plumbing.
package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/wesback/scrobble-backfill/internal/config"
)

// Run executes the command line application using the default configuration
// store and returns a process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	store, err := config.NewDefaultStore()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	return RunWithStore(args, stdout, stderr, store)
}

// RunWithStore executes the command line application with an injected store.
// It is useful for embedding and keeps command behavior independently
// testable without changing the user's configuration.
func RunWithStore(args []string, stdout, stderr io.Writer, store config.Store) int {
	options, command, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n\n", err)
		printUsage(stderr)
		return 2
	}
	if options.help {
		printUsage(stdout)
		return 0
	}
	if len(command) == 0 {
		printUsage(stderr)
		return 2
	}

	switch command[0] {
	case "profile":
		return runProfile(command[1:], options, stdout, stderr, store)
	default:
		fmt.Fprintf(stderr, "error: unknown command %q\n\n", command[0])
		printUsage(stderr)
		return 2
	}
}

type options struct {
	profile string
	help    bool
}

func parseArgs(args []string) (options, []string, error) {
	var options options
	var command []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			command = append(command, args[i+1:]...)
			return options, command, nil
		case arg == "--help", arg == "-h":
			options.help = true
		case arg == "--profile":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" {
				return options, nil, errors.New("--profile requires a non-empty name")
			}
			options.profile = args[i+1]
			i++
		case strings.HasPrefix(arg, "--profile="):
			options.profile = strings.TrimPrefix(arg, "--profile=")
			if strings.TrimSpace(options.profile) == "" {
				return options, nil, errors.New("--profile requires a non-empty name")
			}
		case strings.HasPrefix(arg, "-"):
			return options, nil, fmt.Errorf("unknown option %q", arg)
		default:
			command = append(command, arg)
		}
	}
	return options, command, nil
}

func runProfile(command []string, options options, stdout, stderr io.Writer, store config.Store) int {
	if len(command) == 0 {
		fmt.Fprintln(stderr, "error: profile requires a subcommand")
		fmt.Fprintln(stderr, "usage: rescrobble [--profile <name>] profile use <name>")
		return 2
	}
	if command[0] != "use" {
		fmt.Fprintf(stderr, "error: unknown profile command %q\n", command[0])
		fmt.Fprintln(stderr, "usage: rescrobble [--profile <name>] profile use <name>")
		return 2
	}
	if len(command) != 2 || strings.TrimSpace(command[1]) == "" {
		fmt.Fprintln(stderr, "error: profile use requires a profile name")
		fmt.Fprintln(stderr, "usage: rescrobble [--profile <name>] profile use <name>")
		return 2
	}
	if store == nil {
		fmt.Fprintln(stderr, "error: configuration store is unavailable")
		return 1
	}

	cfg, err := store.Load()
	if err != nil {
		fmt.Fprintf(stderr, "error: load configuration: %v\n", err)
		return 1
	}
	name := command[1]
	if options.profile != "" {
		resolved, err := cfg.ResolveProfileName(options.profile)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			if names := cfg.ProfileNames(); len(names) > 0 {
				fmt.Fprintf(stderr, "configured profiles: %s\n", strings.Join(names, ", "))
			} else {
				fmt.Fprintln(stderr, "configure a profile before selecting it")
			}
			return 1
		}
		name = resolved
	}
	if err := cfg.SetActiveProfile(name); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		if names := cfg.ProfileNames(); len(names) > 0 {
			fmt.Fprintf(stderr, "configured profiles: %s\n", strings.Join(names, ", "))
		} else {
			fmt.Fprintln(stderr, "configure a profile before selecting it")
		}
		return 1
	}
	if err := store.Save(cfg); err != nil {
		fmt.Fprintf(stderr, "error: save configuration: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "active profile set to %q\n", name)
	return 0
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  rescrobble [--profile <name>] profile use <name>")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Global options:")
	fmt.Fprintln(w, "  --profile <name>  select a configured profile for the command")
	fmt.Fprintln(w, "  --help            show this help")
}
