// Package cli defines the Rescrobble command tree and its command-independent
// profile selection plumbing.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wesback/scrobble-backfill/internal/config"
	"github.com/wesback/scrobble-backfill/internal/credentials"
	"github.com/wesback/scrobble-backfill/internal/journal"
	"github.com/wesback/scrobble-backfill/internal/lastfm"
	"github.com/wesback/scrobble-backfill/internal/observability"
	"github.com/wesback/scrobble-backfill/internal/spotify"
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
	return RunWithDependencies(args, stdout, stderr, Dependencies{
		ConfigStore:     store,
		CredentialStore: credentials.NewDefaultStore(),
		LastFMClient:    lastfm.NewClientFromEnv(),
		Input:           os.Stdin,
	})
}

// Dependencies contains the persistent and external boundaries used by
// account commands. Production callers should use Run or RunWithStore;
// injection keeps command behavior testable without weakening the secure
// credential boundary.
type Dependencies struct {
	ConfigStore     config.Store
	CredentialStore credentials.Store
	LastFMClient    *lastfm.Client
	JournalStore    journal.Store
	Submission      lastfm.SubmissionOptions
	Input           io.Reader
}

const secureStoreGuidance = "make an OS-native credential service available (Windows Credential Manager, macOS Keychain, or Linux Secret Service)"

// LargeImportConfirmationThreshold is the largest missing-play count that
// imports submit without an interactive confirmation. Larger imports require
// confirmation unless --yes is supplied.
const LargeImportConfirmationThreshold = 100

// RunWithDependencies executes the command line application with injected
// configuration, credentials, Last.fm client, and input boundaries.
func RunWithDependencies(args []string, stdout, stderr io.Writer, dependencies Dependencies) int {
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
	if command[0] != "analyse" && command[0] != "import" &&
		(options.from != "" || options.to != "" || options.timestampToleranceSet) {
		fmt.Fprintln(stderr, "error: --from, --to, and --timestamp-tolerance are only valid with analyse or import")
		printUsage(stderr)
		return 2
	}
	if command[0] != "import" && (options.dryRun || options.yes) {
		fmt.Fprintln(stderr, "error: --dry-run and --yes are only valid with import")
		printUsage(stderr)
		return 2
	}
	if options.logLevelExplicit {
		logger := observability.NewLogger(stderr, options.logLevel)
		if err := logger.Normal("command.started", map[string]any{"command": command[0]}); err != nil {
			fmt.Fprintf(stderr, "error: write normal log event: %v\n", err)
			return 1
		}
		if err := logger.Verbose("command.options", map[string]any{"command": command[0], "argument_count": len(command) - 1}); err != nil {
			fmt.Fprintf(stderr, "error: write verbose log event: %v\n", err)
			return 1
		}
		if err := logger.Debug("command.debug", map[string]any{"command": command[0]}); err != nil {
			fmt.Fprintf(stderr, "error: write debug log event: %v\n", err)
			return 1
		}
	}

	switch command[0] {
	case "profile":
		return runProfile(command[1:], options, stdout, stderr, dependencies.ConfigStore)
	case "login":
		return runLogin(options, stdout, stderr, dependencies)
	case "logout":
		return runLogout(options, stdout, stderr, dependencies)
	case "status":
		return runStatus(options, stdout, stderr, dependencies)
	case "analyse":
		return runAnalyse(command[1:], options, stdout, stderr, dependencies)
	case "import":
		return runImport(command[1:], options, stdout, stderr, dependencies)
	case "verify":
		return runVerify(command[1:], options, stdout, stderr, dependencies)
	default:
		fmt.Fprintf(stderr, "error: unknown command %q\n\n", command[0])
		printUsage(stderr)
		return 2
	}
}

type options struct {
	profile               string
	logLevel              observability.Level
	logLevelExplicit      bool
	help                  bool
	from                  string
	to                    string
	timestampTolerance    time.Duration
	timestampToleranceSet bool
	dryRun                bool
	yes                   bool
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
		case arg == "--verbose":
			options.logLevel = observability.LevelVerbose
			options.logLevelExplicit = true
		case arg == "--debug":
			options.logLevel = observability.LevelDebug
			options.logLevelExplicit = true
		case arg == "--log-level":
			if i+1 >= len(args) {
				return options, nil, errors.New("--log-level requires normal, verbose, or debug")
			}
			level, err := observability.ParseLevel(args[i+1])
			if err != nil {
				return options, nil, err
			}
			options.logLevel = level
			options.logLevelExplicit = true
			i++
		case strings.HasPrefix(arg, "--log-level="):
			level, err := observability.ParseLevel(strings.TrimPrefix(arg, "--log-level="))
			if err != nil {
				return options, nil, err
			}
			options.logLevel = level
			options.logLevelExplicit = true
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
		case arg == "--dry-run":
			options.dryRun = true
		case arg == "--yes":
			options.yes = true
		case arg == "--from", arg == "--to", arg == "--timestamp-tolerance":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" {
				return options, nil, fmt.Errorf("%s requires a value", arg)
			}
			if err := setAnalysisOption(&options, arg, args[i+1]); err != nil {
				return options, nil, err
			}
			i++
		case strings.HasPrefix(arg, "--from="), strings.HasPrefix(arg, "--to="), strings.HasPrefix(arg, "--timestamp-tolerance="):
			name, value, _ := strings.Cut(arg, "=")
			if strings.TrimSpace(value) == "" {
				return options, nil, fmt.Errorf("%s requires a value", name)
			}
			if err := setAnalysisOption(&options, name, value); err != nil {
				return options, nil, err
			}
		case strings.HasPrefix(arg, "-"):
			return options, nil, fmt.Errorf("unknown option %q", arg)
		default:
			command = append(command, arg)
		}
	}
	return options, command, nil
}

func setAnalysisOption(options *options, name, value string) error {
	switch name {
	case "--from":
		options.from = value
	case "--to":
		options.to = value
	case "--timestamp-tolerance":
		tolerance, err := parseTimestampTolerance(value)
		if err != nil {
			return err
		}
		options.timestampTolerance = tolerance
		options.timestampToleranceSet = true
	default:
		return fmt.Errorf("unknown analysis option %q", name)
	}
	return nil
}

func parseTimestampTolerance(value string) (time.Duration, error) {
	tolerance, err := time.ParseDuration(strings.TrimSpace(value))
	if err == nil {
		if tolerance < 0 {
			return 0, errors.New("--timestamp-tolerance must not be negative")
		}
		return tolerance, nil
	}
	seconds, integerErr := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if integerErr == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second, nil
	}
	return 0, fmt.Errorf("--timestamp-tolerance must be a non-negative duration (for example 60s): %w", err)
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

func runLogin(options options, stdout, stderr io.Writer, dependencies Dependencies) int {
	if dependencies.ConfigStore == nil {
		fmt.Fprintln(stderr, "error: configuration store is unavailable")
		return 1
	}
	if dependencies.CredentialStore == nil {
		fmt.Fprintf(stderr, "error: secure credential store is unavailable; %s\n", secureStoreGuidance)
		return 1
	}
	if dependencies.LastFMClient == nil {
		fmt.Fprintln(stderr, "error: Last.fm client is unavailable")
		return 1
	}
	cfg, err := dependencies.ConfigStore.Load()
	if err != nil {
		fmt.Fprintf(stderr, "error: load configuration: %v\n", err)
		return 1
	}
	profileName, err := resolveLoginProfile(cfg, options.profile)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	previousSession, previousErr := dependencies.CredentialStore.Load(profileName)
	if previousErr != nil && !errors.Is(previousErr, credentials.ErrCredentialNotFound) {
		printCredentialError(stderr, "load existing credential", previousErr)
		return 1
	}
	session, err := dependencies.LastFMClient.Authenticate(context.Background(), stdout, dependencies.Input)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if err := dependencies.CredentialStore.Save(profileName, session.Key); err != nil {
		printCredentialError(stderr, "save credential", err)
		return 1
	}

	if cfg.Profiles == nil {
		cfg.Profiles = make(map[string]config.Profile)
	}
	profile := cfg.Profiles[profileName]
	profile.Name = profileName
	if session.Name != "" {
		profile.LastFMUsername = session.Name
	}
	cfg.Profiles[profileName] = profile
	if cfg.ActiveProfile == "" {
		cfg.ActiveProfile = profileName
	}
	if err := dependencies.ConfigStore.Save(cfg); err != nil {
		rollbackErr := rollbackCredential(dependencies.CredentialStore, profileName, previousSession, previousErr)
		if rollbackErr != nil {
			fmt.Fprintf(stderr, "error: save configuration: %v; credential rollback failed: %v\n", err, rollbackErr)
		} else {
			fmt.Fprintf(stderr, "error: save configuration: %v\n", err)
		}
		return 1
	}
	fmt.Fprintf(stdout, "logged in to profile %q\n", profileName)
	return 0
}

func runLogout(options options, stdout, stderr io.Writer, dependencies Dependencies) int {
	profileName, err := resolveExistingProfile(options, dependencies.ConfigStore)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if dependencies.CredentialStore == nil {
		fmt.Fprintf(stderr, "error: secure credential store is unavailable; %s\n", secureStoreGuidance)
		return 1
	}
	if err := dependencies.CredentialStore.Delete(profileName); err != nil {
		printCredentialError(stderr, fmt.Sprintf("delete %q credential", profileName), err)
		return 1
	}
	fmt.Fprintf(stdout, "logged out of profile %q\n", profileName)
	return 0
}

func runStatus(options options, stdout, stderr io.Writer, dependencies Dependencies) int {
	profileName, err := resolveExistingProfile(options, dependencies.ConfigStore)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if dependencies.CredentialStore == nil {
		fmt.Fprintf(stderr, "error: secure credential store is unavailable; %s\n", secureStoreGuidance)
		return 1
	}
	hasSession, err := dependencies.CredentialStore.Has(profileName)
	if err != nil {
		printCredentialError(stderr, fmt.Sprintf("check %q credential", profileName), err)
		return 1
	}
	if hasSession {
		fmt.Fprintf(stdout, "profile %q: logged in\n", profileName)
	} else {
		fmt.Fprintf(stdout, "profile %q: not logged in\n", profileName)
	}
	return 0
}

func callAuthenticated(ctx context.Context, options options, method string, params map[string]string, dependencies Dependencies) (map[string]any, error) {
	if dependencies.LastFMClient == nil {
		return nil, errors.New("Last.fm client is unavailable")
	}
	if dependencies.CredentialStore == nil {
		return nil, errors.New("secure credential store is unavailable")
	}
	profileName, err := resolveExistingProfile(options, dependencies.ConfigStore)
	if err != nil {
		return nil, err
	}
	sessionKey, err := dependencies.CredentialStore.Load(profileName)
	if errors.Is(err, credentials.ErrCredentialNotFound) {
		return nil, fmt.Errorf("profile %q is not logged in", profileName)
	}
	if err != nil {
		return nil, fmt.Errorf("load %q credential: %w", profileName, err)
	}
	if strings.TrimSpace(sessionKey) == "" {
		return nil, fmt.Errorf("stored %q credential is empty", profileName)
	}
	return dependencies.LastFMClient.Call(ctx, method, params, sessionKey)
}

func resolveExistingProfile(options options, store config.Store) (string, error) {
	if store == nil {
		return "", errors.New("configuration store is unavailable")
	}
	cfg, err := store.Load()
	if err != nil {
		return "", fmt.Errorf("load configuration: %w", err)
	}
	name, err := cfg.ResolveProfileName(options.profile)
	if err != nil {
		return "", err
	}
	return name, nil
}

func runAnalyse(command []string, options options, stdout, stderr io.Writer, dependencies Dependencies) int {
	if len(command) == 0 {
		fmt.Fprintln(stderr, "error: analyse requires at least one Spotify export input")
		fmt.Fprintln(stderr, "usage: rescrobble [options] analyse [--from YYYY-MM-DD] [--to YYYY-MM-DD] [--timestamp-tolerance duration] <export>...")
		return 2
	}

	if dependencies.ConfigStore == nil {
		fmt.Fprintln(stderr, "error: configuration store is unavailable")
		return 1
	}
	if dependencies.CredentialStore == nil {
		fmt.Fprintf(stderr, "error: secure credential store is unavailable; %s\n", secureStoreGuidance)
		return 1
	}
	if dependencies.LastFMClient == nil {
		fmt.Fprintln(stderr, "error: Last.fm client is unavailable")
		return 1
	}

	cfg, err := dependencies.ConfigStore.Load()
	if err != nil {
		fmt.Fprintf(stderr, "error: load configuration: %v\n", err)
		return 1
	}
	profileName, err := cfg.ResolveProfileName(options.profile)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	profile := cfg.Profiles[profileName]
	sessionKey, err := dependencies.CredentialStore.Load(profileName)
	if errors.Is(err, credentials.ErrCredentialNotFound) {
		fmt.Fprintf(stderr, "error: profile %q is not logged in\n", profileName)
		return 1
	}
	if err != nil {
		printCredentialError(stderr, fmt.Sprintf("load %q credential", profileName), err)
		return 1
	}
	if strings.TrimSpace(sessionKey) == "" {
		fmt.Fprintf(stderr, "error: stored %q credential is empty\n", profileName)
		return 1
	}
	if strings.TrimSpace(profile.LastFMUsername) == "" {
		fmt.Fprintf(stderr, "error: profile %q has no Last.fm username; log in again\n", profileName)
		return 1
	}

	location := time.Local
	from, err := parseAnalysisDate(options.from, location)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	to, err := parseAnalysisDate(options.to, location)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	paths := append([]string(nil), command...)
	if from.IsZero() || to.IsZero() {
		discoveredFrom, discoveredTo, discoverErr := discoverAnalysisBounds(paths, location)
		if discoverErr != nil {
			fmt.Fprintf(stderr, "error: %v\n", discoverErr)
			return 1
		}
		if from.IsZero() {
			from = discoveredFrom
		}
		if to.IsZero() {
			to = discoveredTo
		}
	}

	var ingestionSummary spotify.Summary
	exclusionCounts := make(map[string]int)
	summary, err := lastfm.Compare(
		context.Background(),
		dependencies.LastFMClient,
		lastfm.AuthenticatedProfile{Username: profile.LastFMUsername, SessionKey: sessionKey},
		lastfm.ComparisonRequest{
			From:                  from,
			To:                    to,
			Timezone:              location,
			TimestampTolerance:    options.timestampTolerance,
			TimestampToleranceSet: options.timestampToleranceSet,
			Plays: func(ctx context.Context, consume spotify.Consumer) error {
				var ingestErr error
				ingestionSummary, ingestErr = spotify.IngestFiles(ctx, paths, consume, func(warning spotify.Warning) {
					exclusionCounts[warning.Code]++
				})
				return ingestErr
			},
		},
		func(lastfm.ComparisonResult) error { return nil },
	)
	if err != nil {
		fmt.Fprintf(stderr, "error: analyse: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Analysis summary for profile %q\n", profileName)
	fmt.Fprintf(stdout, "Total Spotify plays: %d\n", ingestionSummary.Records)
	fmt.Fprintf(stdout, "In-scope Last.fm scrobbles: %d\n", summary.LastFMScrobbles)
	fmt.Fprintf(stdout, "Estimated missing plays: %d\n", summary.Missing)
	fmt.Fprintf(stdout, "Covered date range: %s to %s\n", from.In(location).Format("2006-01-02"), to.In(location).Format("2006-01-02"))
	fmt.Fprintf(stdout, "Timestamp tolerance: %s\n", summary.TimestampTolerance)
	fmt.Fprintf(stdout, "Confidence: high=%d medium=%d low=%d\n", summary.HighConfidence, summary.MediumConfidence, summary.LowConfidence)
	for reason, count := range summary.ExcludedByReason {
		exclusionCounts[reason] += count
	}
	fmt.Fprintln(stdout, "Source exclusion reasons:")
	reasons := make([]string, 0, len(exclusionCounts))
	for reason := range exclusionCounts {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	for _, reason := range reasons {
		fmt.Fprintf(stdout, "  %s: %d\n", reason, exclusionCounts[reason])
	}
	return 0
}

func runImport(command []string, options options, stdout, stderr io.Writer, dependencies Dependencies) int {
	if len(command) == 0 {
		fmt.Fprintln(stderr, "error: import requires at least one Spotify export input")
		fmt.Fprintln(stderr, "usage: rescrobble [options] import [--from YYYY-MM-DD] [--to YYYY-MM-DD] [--dry-run] [--yes] <export>...")
		return 2
	}
	if dependencies.ConfigStore == nil {
		fmt.Fprintln(stderr, "error: configuration store is unavailable")
		return 1
	}
	if dependencies.CredentialStore == nil {
		fmt.Fprintf(stderr, "error: secure credential store is unavailable; %s\n", secureStoreGuidance)
		return 1
	}
	if dependencies.LastFMClient == nil {
		fmt.Fprintln(stderr, "error: Last.fm client is unavailable")
		return 1
	}

	cfg, err := dependencies.ConfigStore.Load()
	if err != nil {
		fmt.Fprintf(stderr, "error: load configuration: %v\n", err)
		return 1
	}
	profileName, err := cfg.ResolveProfileName(options.profile)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	profile := cfg.Profiles[profileName]
	sessionKey, err := dependencies.CredentialStore.Load(profileName)
	if errors.Is(err, credentials.ErrCredentialNotFound) {
		fmt.Fprintf(stderr, "error: profile %q is not logged in\n", profileName)
		return 1
	}
	if err != nil {
		printCredentialError(stderr, fmt.Sprintf("load %q credential", profileName), err)
		return 1
	}
	if strings.TrimSpace(sessionKey) == "" {
		fmt.Fprintf(stderr, "error: stored %q credential is empty\n", profileName)
		return 1
	}
	if strings.TrimSpace(profile.LastFMUsername) == "" {
		fmt.Fprintf(stderr, "error: profile %q has no Last.fm username; log in again\n", profileName)
		return 1
	}

	location := time.Local
	from, err := parseAnalysisDate(options.from, location)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	to, err := parseAnalysisDate(options.to, location)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	paths := append([]string(nil), command...)
	if from.IsZero() || to.IsZero() {
		discoveredFrom, discoveredTo, discoverErr := discoverAnalysisBounds(paths, location)
		if discoverErr != nil {
			fmt.Fprintf(stderr, "error: %v\n", discoverErr)
			return 1
		}
		if from.IsZero() {
			from = discoveredFrom
		}
		if to.IsZero() {
			to = discoveredTo
		}
	}

	var ingestionSummary spotify.Summary
	missing := make([]spotify.Play, 0)
	summary, err := lastfm.Compare(
		context.Background(),
		dependencies.LastFMClient,
		lastfm.AuthenticatedProfile{Username: profile.LastFMUsername, SessionKey: sessionKey},
		lastfm.ComparisonRequest{
			From:                  from,
			To:                    to,
			Timezone:              location,
			TimestampTolerance:    options.timestampTolerance,
			TimestampToleranceSet: options.timestampToleranceSet,
			Plays: func(ctx context.Context, consume spotify.Consumer) error {
				var ingestErr error
				ingestionSummary, ingestErr = spotify.IngestFiles(ctx, paths, consume, func(warning spotify.Warning) {
					fmt.Fprintf(stderr, "warning: %s: %s\n", warning.Code, warning.Reason)
				})
				return ingestErr
			},
		},
		func(result lastfm.ComparisonResult) error {
			if result.Status == lastfm.ComparisonStatusMissing {
				missing = append(missing, result.Play)
			}
			return nil
		},
	)
	if err != nil {
		fmt.Fprintf(stderr, "error: import comparison: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Import summary for profile %q\n", profileName)
	fmt.Fprintf(stdout, "Total Spotify plays: %d\n", ingestionSummary.Records)
	fmt.Fprintf(stdout, "Skipped duplicates: %d\n", summary.Matched)
	fmt.Fprintf(stdout, "Missing plays: %d\n", summary.Missing)
	fmt.Fprintf(stdout, "Covered date range: %s to %s\n", from.In(location).Format("2006-01-02"), to.In(location).Format("2006-01-02"))
	fmt.Fprintf(stdout, "Timestamp tolerance: %s\n", summary.TimestampTolerance)
	if options.dryRun {
		fmt.Fprintln(stdout, "Dry run: no Last.fm submissions will be sent.")
	}

	if len(missing) == 0 {
		fmt.Fprintln(stdout, "Nothing to submit.")
		return 0
	}
	if len(missing) > LargeImportConfirmationThreshold && !options.dryRun && !options.yes {
		fmt.Fprintf(stdout, "Importing more than %d missing plays requires confirmation. Continue? [y/N] ", LargeImportConfirmationThreshold)
		input := dependencies.Input
		if input == nil {
			input = os.Stdin
		}
		answer, readErr := bufio.NewReader(input).ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			fmt.Fprintf(stderr, "error: read import confirmation: %v\n", readErr)
			return 1
		}
		answer = strings.TrimSpace(strings.ToLower(answer))
		if answer != "y" && answer != "yes" {
			fmt.Fprintln(stdout, "Import cancelled.")
			return 1
		}
	}

	store := dependencies.JournalStore
	if store == nil {
		store, err = journal.NewDefaultStore()
		if err != nil {
			fmt.Fprintf(stderr, "error: create import journal: %v\n", err)
			return 1
		}
	}
	invocationID := fmt.Sprintf("import-%d", time.Now().UTC().UnixNano())
	run, err := store.CreateRun(profileName, invocationID)
	if err != nil {
		fmt.Fprintf(stderr, "error: create import journal run: %v\n", err)
		return 1
	}
	service := lastfm.NewSubmissionService(dependencies.LastFMClient, store, dependencies.Submission)
	authenticated := lastfm.AuthenticatedProfile{Username: profile.LastFMUsername, SessionKey: sessionKey}
	if options.dryRun {
		if err := service.PlanSpotify(context.Background(), run, missing); err != nil {
			fmt.Fprintf(stderr, "error: plan import batches: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "Planned missing plays in the import journal; no batches submitted.")
		return 0
	}
	if err := service.SubmitSpotify(context.Background(), authenticated, run, missing); err != nil {
		fmt.Fprintf(stderr, "error: submit import batches: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Submitted %d missing plays.\n", len(missing))
	return 0
}

func runVerify(command []string, options options, stdout, stderr io.Writer, dependencies Dependencies) int {
	if dependencies.ConfigStore == nil {
		fmt.Fprintln(stderr, "error: configuration store is unavailable")
		return 1
	}
	if dependencies.CredentialStore == nil {
		fmt.Fprintf(stderr, "error: secure credential store is unavailable; %s\n", secureStoreGuidance)
		return 1
	}
	if dependencies.LastFMClient == nil {
		fmt.Fprintln(stderr, "error: Last.fm client is unavailable")
		return 1
	}

	cfg, err := dependencies.ConfigStore.Load()
	if err != nil {
		fmt.Fprintf(stderr, "error: load configuration: %v\n", err)
		return 1
	}
	profileName, err := cfg.ResolveProfileName(options.profile)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	profile := cfg.Profiles[profileName]
	if strings.TrimSpace(profile.LastFMUsername) == "" {
		fmt.Fprintf(stderr, "error: profile %q has no Last.fm username; log in again\n", profileName)
		return 1
	}
	sessionKey, err := dependencies.CredentialStore.Load(profileName)
	if errors.Is(err, credentials.ErrCredentialNotFound) {
		fmt.Fprintf(stderr, "error: profile %q is not logged in\n", profileName)
		return 1
	}
	if err != nil {
		printCredentialError(stderr, fmt.Sprintf("load %q credential", profileName), err)
		return 1
	}
	if strings.TrimSpace(sessionKey) == "" {
		fmt.Fprintf(stderr, "error: stored %q credential is empty\n", profileName)
		return 1
	}

	store := dependencies.JournalStore
	if store == nil {
		store, err = journal.NewDefaultStore()
		if err != nil {
			fmt.Fprintf(stderr, "error: create verification journal: %v\n", err)
			return 1
		}
	}
	runs, err := store.ListRuns(profileName)
	if err != nil {
		fmt.Fprintf(stderr, "error: read verification journal: %v\n", err)
		return 1
	}
	selected := make(map[string]struct{}, len(command))
	for _, invocationID := range command {
		if strings.TrimSpace(invocationID) == "" {
			fmt.Fprintln(stderr, "error: verify run identity must not be empty")
			return 2
		}
		if _, duplicate := selected[invocationID]; duplicate {
			fmt.Fprintf(stderr, "error: verify run %q was selected more than once\n", invocationID)
			return 2
		}
		selected[invocationID] = struct{}{}
	}

	authenticated := lastfm.AuthenticatedProfile{
		Username:   profile.LastFMUsername,
		SessionKey: sessionKey,
	}
	verifiedRuns := 0
	totalSubmitted := 0
	totalConfirmed := 0
	totalMissing := 0
	for _, run := range runs {
		if len(selected) > 0 {
			if _, ok := selected[run.InvocationID]; !ok {
				continue
			}
		}
		verifiedRuns++
		submissions := submittedPayloads(run)
		fmt.Fprintf(stdout, "Verification for run %q\n", run.InvocationID)
		if len(submissions) == 0 {
			fmt.Fprintln(stdout, "  No submitted journaled scrobbles.")
			continue
		}
		summary, results, verifyErr := lastfm.VerifySubmissions(
			context.Background(),
			dependencies.LastFMClient,
			authenticated,
			submissions,
		)
		if verifyErr != nil {
			fmt.Fprintf(stderr, "error: verify run %q: %v\n", run.InvocationID, verifyErr)
			return 1
		}
		fmt.Fprintf(stdout, "  Submitted journaled scrobbles: %d\n", summary.Submitted)
		fmt.Fprintf(stdout, "  Confirmed journaled scrobbles: %d\n", summary.Confirmed)
		fmt.Fprintf(stdout, "  Absent journaled scrobbles: %d\n", summary.Missing)
		for _, result := range results {
			if result.Status != lastfm.ComparisonStatusMissing {
				continue
			}
			fmt.Fprintf(stdout, "  Absent: %q - %q at %s\n",
				result.Submission.Artist,
				result.Submission.Track,
				result.Submission.Timestamp.UTC().Format(time.RFC3339),
			)
		}
		totalSubmitted += summary.Submitted
		totalConfirmed += summary.Confirmed
		totalMissing += summary.Missing
	}
	if len(selected) > 0 && verifiedRuns == 0 {
		fmt.Fprintf(stderr, "error: no journal run found for selected invocation(s): %s\n", strings.Join(command, ", "))
		return 1
	}
	if verifiedRuns == 0 {
		fmt.Fprintf(stdout, "No journal runs with submitted scrobbles found for profile %q.\n", profileName)
		return 0
	}
	if verifiedRuns > 1 {
		fmt.Fprintf(stdout, "Verification total: submitted=%d confirmed=%d absent=%d\n", totalSubmitted, totalConfirmed, totalMissing)
	}
	return 0
}

func submittedPayloads(run journal.Run) []journal.Submission {
	var submissions []journal.Submission
	for _, batch := range run.Batches {
		if batch.State != journal.StateSubmitted {
			continue
		}
		submissions = append(submissions, batch.Payloads...)
	}
	return submissions
}

func parseAnalysisDate(value string, location *time.Location) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	date, err := time.ParseInLocation("2006-01-02", value, location)
	if err != nil {
		return time.Time{}, fmt.Errorf("analysis date %q must use YYYY-MM-DD", value)
	}
	return date, nil
}

func discoverAnalysisBounds(paths []string, location *time.Location) (time.Time, time.Time, error) {
	var first, last time.Time
	_, err := spotify.IngestFiles(context.Background(), paths, func(play spotify.Play) error {
		local := play.Timestamp.In(location)
		day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
		if first.IsZero() || day.Before(first) {
			first = day
		}
		if last.IsZero() || day.After(last) {
			last = day
		}
		return nil
	}, nil)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("read Spotify exports: %w", err)
	}
	if first.IsZero() || last.IsZero() {
		return time.Time{}, time.Time{}, errors.New("cannot determine analysis date range from Spotify exports; pass --from and --to")
	}
	return first, last, nil
}

func printCredentialError(stderr io.Writer, action string, err error) {
	if errors.Is(err, credentials.ErrSecureStoreUnavailable) {
		fmt.Fprintf(stderr, "error: %s: %v; %s\n", action, credentials.ErrSecureStoreUnavailable, secureStoreGuidance)
		return
	}
	fmt.Fprintf(stderr, "error: %s: %v\n", action, err)
}

func resolveLoginProfile(cfg config.Config, explicit string) (string, error) {
	if name := strings.TrimSpace(explicit); name != "" {
		return name, nil
	}
	if name := strings.TrimSpace(cfg.ActiveProfile); name != "" {
		return name, nil
	}
	return "default", nil
}

func rollbackCredential(store credentials.Store, profile, previous string, previousErr error) error {
	if previousErr == nil {
		return store.Save(profile, previous)
	}
	return store.Delete(profile)
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  rescrobble [--profile <name>] login")
	fmt.Fprintln(w, "  rescrobble [--profile <name>] logout")
	fmt.Fprintln(w, "  rescrobble [--profile <name>] status")
	fmt.Fprintln(w, "  rescrobble [--profile <name>] analyse [--from YYYY-MM-DD] [--to YYYY-MM-DD] [--timestamp-tolerance duration] <export>...")
	fmt.Fprintln(w, "  rescrobble [--profile <name>] import [--from YYYY-MM-DD] [--to YYYY-MM-DD] [--dry-run] [--yes] <export>...")
	fmt.Fprintln(w, "  rescrobble [--profile <name>] verify [<invocation-id>...]")
	fmt.Fprintf(w, "  import confirms interactively when more than %d missing plays would be submitted; --yes bypasses confirmation.\n", LargeImportConfirmationThreshold)
	fmt.Fprintln(w, "  rescrobble [--profile <name>] profile use <name>")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Global options:")
	fmt.Fprintln(w, "  --profile <name>  select a configured profile for the command")
	fmt.Fprintln(w, "  --log-level <level>  set logging to normal, verbose, or debug")
	fmt.Fprintln(w, "  --verbose         enable verbose logging")
	fmt.Fprintln(w, "  --debug           enable debug logging")
	fmt.Fprintln(w, "  --help            show this help")
}
