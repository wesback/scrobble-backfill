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
	"github.com/wesback/scrobble-backfill/internal/report"
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
	credentialStore, err := credentials.NewDefaultStore()
	if err != nil {
		fmt.Fprintf(stderr, "error: initialize credential store: %v\n", err)
		return 1
	}
	return RunWithDependencies(args, stdout, stderr, Dependencies{
		ConfigStore:     store,
		CredentialStore: credentialStore,
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

const secureStoreGuidance = "make an OS-native credential service available (Windows Credential Manager, macOS Keychain, or Linux Secret Service); on Linux, the encrypted local fallback is selected only when Secret Service is unavailable and provides weaker protection"

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
	if command[0] == "report" {
		if options.reportFormatCount != 1 {
			fmt.Fprintln(stderr, "error: report requires exactly one of --json, --csv, or --html")
			printUsage(stderr)
			return 2
		}
	} else if options.reportFormatCount != 0 {
		fmt.Fprintln(stderr, "error: --json, --csv, and --html are only valid with report")
		printUsage(stderr)
		return 2
	}
	if command[0] != "analyse" && command[0] != "import" && command[0] != "report" &&
		(options.from != "" || options.to != "" || options.timestampToleranceSet) {
		fmt.Fprintln(stderr, "error: --from, --to, and --timestamp-tolerance are only valid with analyse, import, or report")
		printUsage(stderr)
		return 2
	}
	if command[0] != "import" && command[0] != "report" && options.batchDelaySet {
		fmt.Fprintln(stderr, "error: --batch-delay is only valid with import or report")
		printUsage(stderr)
		return 2
	}
	if command[0] != "import" && (options.dryRun || options.yes) {
		fmt.Fprintln(stderr, "error: --dry-run and --yes are only valid with import")
		printUsage(stderr)
		return 2
	}
	logger := observability.NewLogger(stderr, options.logLevel)
	if options.logLevelExplicit {
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
		return runLogin(options, stdout, stderr, dependencies, logger)
	case "logout":
		return runLogout(options, stdout, stderr, dependencies)
	case "status":
		return runStatus(options, stdout, stderr, dependencies)
	case "doctor":
		return runDoctor(options, stdout, stderr, dependencies)
	case "analyse":
		return runAnalyse(command[1:], options, stdout, stderr, dependencies)
	case "import":
		return runImport(command[1:], options, stdout, stderr, dependencies)
	case "verify":
		return runVerify(command[1:], options, stdout, stderr, dependencies)
	case "report":
		return runReport(command[1:], options, stdout, stderr, dependencies)
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
	batchDelay            time.Duration
	batchDelaySet         bool
	reportFormat          report.Format
	reportFormatCount     int
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
		case arg == "--json":
			options.reportFormat = report.FormatJSON
			options.reportFormatCount++
		case arg == "--csv":
			options.reportFormat = report.FormatCSV
			options.reportFormatCount++
		case arg == "--html":
			options.reportFormat = report.FormatHTML
			options.reportFormatCount++
		case arg == "--from", arg == "--to", arg == "--timestamp-tolerance", arg == "--batch-delay":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" {
				return options, nil, fmt.Errorf("%s requires a value", arg)
			}
			if err := setOperationalOption(&options, arg, args[i+1]); err != nil {
				return options, nil, err
			}
			i++
		case strings.HasPrefix(arg, "--from="), strings.HasPrefix(arg, "--to="), strings.HasPrefix(arg, "--timestamp-tolerance="), strings.HasPrefix(arg, "--batch-delay="):
			name, value, _ := strings.Cut(arg, "=")
			if strings.TrimSpace(value) == "" {
				return options, nil, fmt.Errorf("%s requires a value", name)
			}
			if err := setOperationalOption(&options, name, value); err != nil {
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

func setOperationalOption(options *options, name, value string) error {
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
	case "--batch-delay":
		delay, err := parseBatchDelay(value)
		if err != nil {
			return err
		}
		options.batchDelay = delay
		options.batchDelaySet = true
	default:
		return fmt.Errorf("unknown operational option %q", name)
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
		return durationFromSeconds(seconds, "--timestamp-tolerance")
	}
	return 0, fmt.Errorf("--timestamp-tolerance must be a non-negative duration (for example 60s): %w", err)
}

func parseBatchDelay(value string) (time.Duration, error) {
	delay, err := time.ParseDuration(strings.TrimSpace(value))
	if err == nil {
		if delay < 0 {
			return 0, errors.New("--batch-delay must not be negative")
		}
		return delay, nil
	}
	seconds, integerErr := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if integerErr == nil && seconds >= 0 {
		return durationFromSeconds(seconds, "--batch-delay")
	}
	return 0, fmt.Errorf("--batch-delay must be a non-negative duration (for example 1s): %w", err)
}

func durationFromSeconds(seconds int64, option string) (time.Duration, error) {
	const maxSeconds = int64((1<<63 - 1) / int64(time.Second))
	if seconds > maxSeconds {
		return 0, fmt.Errorf("%s seconds value overflows time.Duration", option)
	}
	return time.Duration(seconds) * time.Second, nil
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

func runLogin(options options, stdout, stderr io.Writer, dependencies Dependencies, logger *observability.Logger) int {
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
	dependencies.LastFMClient.Logger = logger
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

type doctorResult struct {
	name string
	err  error
}

var errDoctorCredentialEmpty = errors.New("stored credential is empty")

func runDoctor(options options, stdout, stderr io.Writer, dependencies Dependencies) int {
	fmt.Fprintln(stdout, "Rescrobble doctor")

	cfg, configErr := loadDoctorConfig(dependencies.ConfigStore)
	printDoctorResult(stdout, doctorResult{name: "configuration readability", err: configErr})
	if configErr != nil {
		printDoctorDependentFailures(stdout, "profile selection", "configuration is unreadable")
		printDoctorDependentFailures(stdout, "secure keyring availability", "configuration is unreadable")
		printDoctorDependentFailures(stdout, "credential storage tier", "configuration is unreadable")
		printDoctorDependentFailures(stdout, "credential presence and validity", "configuration is unreadable")
		printDoctorDependentFailures(stdout, "Last.fm API reachability and authentication", "configuration is unreadable")
		printDoctorDependentFailures(stdout, "journal readability", "configuration is unreadable")
		return 1
	}

	profileName, err := cfg.ResolveProfileName(options.profile)
	if err != nil {
		printDoctorDependentFailures(stdout, "profile selection", err.Error())
		printDoctorDependentFailures(stdout, "secure keyring availability", "no profile was selected")
		printDoctorDependentFailures(stdout, "credential storage tier", "no profile was selected")
		printDoctorDependentFailures(stdout, "credential presence and validity", "no profile was selected")
		printDoctorDependentFailures(stdout, "Last.fm API reachability and authentication", "no profile was selected")
		printDoctorDependentFailures(stdout, "journal readability", "no profile was selected")
		return 1
	}
	fmt.Fprintf(stdout, "Profile: %q\n", profileName)

	sessionKey, tier, nativeStoreErr, credentialErr := loadDoctorCredential(dependencies.CredentialStore, profileName)
	tierName, tierErr := doctorTierName(tier)
	printDoctorResult(stdout, doctorResult{name: tierName, err: tierErr})
	keyringErr := doctorKeyringError(nativeStoreErr)
	printDoctorResult(stdout, doctorResult{name: "secure keyring availability", err: keyringErr})

	profile := cfg.Profiles[profileName]
	apiErr := doctorAPIError(dependencies.LastFMClient, profile.LastFMUsername, sessionKey, credentialErr)
	if credentialErr != nil {
		printDoctorResult(stdout, doctorResult{name: "credential presence and validity", err: credentialDiagnosticError(credentialErr)})
	} else if apiErr != nil && strings.Contains(strings.ToLower(apiErr.Error()), "authentication") {
		printDoctorResult(stdout, doctorResult{name: "credential presence and validity", err: errors.New("stored credential was rejected; run login again")})
	} else if apiErr != nil {
		printDoctorResult(stdout, doctorResult{name: "credential presence and validity", err: errors.New("credential is present but its validity could not be checked")})
	} else {
		printDoctorResult(stdout, doctorResult{name: "credential presence and validity"})
	}
	printDoctorResult(stdout, doctorResult{name: "Last.fm API reachability and authentication", err: apiErr})

	journalErr := checkDoctorJournal(dependencies.JournalStore, profileName)
	printDoctorResult(stdout, doctorResult{name: "journal readability", err: journalErr})

	if keyringErr != nil || tierErr != nil || credentialErr != nil || apiErr != nil || journalErr != nil {
		return 1
	}
	return 0
}

func loadDoctorConfig(store config.Store) (config.Config, error) {
	if store == nil {
		return config.Config{}, errors.New("configuration store is unavailable")
	}
	cfg, err := store.Load()
	if err != nil {
		return config.Config{}, fmt.Errorf("read configuration: %w", err)
	}
	return cfg, nil
}

func loadDoctorCredential(store credentials.Store, profile string) (string, credentials.Tier, error, error) {
	if store == nil {
		return "", "", credentials.ErrSecureStoreUnavailable, credentials.ErrSecureStoreUnavailable
	}
	if tiered, ok := store.(credentials.TieredStore); ok {
		session, tier, nativeErr, err := tiered.LoadWithTier(profile)
		if err != nil {
			return "", tier, nativeErr, err
		}
		if strings.TrimSpace(session) == "" {
			return "", tier, nativeErr, errDoctorCredentialEmpty
		}
		return session, tier, nativeErr, nil
	}
	session, err := store.Load(profile)
	if err != nil {
		return "", credentials.TierNative, err, err
	}
	if strings.TrimSpace(session) == "" {
		return "", credentials.TierNative, nil, errDoctorCredentialEmpty
	}
	return session, credentials.TierNative, nil, nil
}

func doctorTierName(tier credentials.Tier) (string, error) {
	switch tier {
	case credentials.TierNative:
		return "credential storage tier: native OS credential store", nil
	case credentials.TierLinuxEncryptedFallback:
		return "credential storage tier: Linux encrypted local fallback (weaker than a native keyring; a user with access to the same host account or root can derive its key)", nil
	default:
		return "credential storage tier", errors.New("selected credential tier could not be determined")
	}
}

func doctorKeyringError(err error) error {
	if err == nil || errors.Is(err, credentials.ErrCredentialNotFound) || errors.Is(err, errDoctorCredentialEmpty) {
		return nil
	}
	if errors.Is(err, credentials.ErrSecureStoreUnavailable) {
		return fmt.Errorf("%s; %s", credentials.ErrSecureStoreUnavailable, secureStoreGuidance)
	}
	return errors.New("secure credential store could not be checked")
}

func credentialDiagnosticError(err error) error {
	switch {
	case errors.Is(err, credentials.ErrCredentialNotFound):
		return errors.New("no stored credential; run login")
	case errors.Is(err, credentials.ErrSecureStoreUnavailable):
		return errors.New("secure credential store is unavailable")
	default:
		return errors.New("could not read stored credential; check the secure credential service")
	}
}

func doctorAPIError(client *lastfm.Client, username, sessionKey string, credentialErr error) error {
	if credentialErr != nil {
		return errors.New("not checked because the profile credential is unavailable")
	}
	if strings.TrimSpace(username) == "" {
		return errors.New("authentication cannot be checked; profile has no Last.fm username")
	}
	if client == nil {
		return errors.New("Last.fm API client is unavailable; configure Last.fm API credentials")
	}
	_, err := client.Call(context.Background(), "user.getInfo", map[string]string{"user": username}, sessionKey)
	if err == nil {
		return nil
	}
	var apiErr *lastfm.APIError
	if errors.As(err, &apiErr) {
		if apiErr.Code == 9 {
			return fmt.Errorf("Last.fm API authentication failed; run login again: %s", redactDiagnosticError(err, sessionKey))
		}
		return fmt.Errorf("Last.fm API returned an error: %s", redactDiagnosticError(err, sessionKey))
	}
	var httpErr *lastfm.HTTPError
	if errors.As(err, &httpErr) {
		return fmt.Errorf("Last.fm API is unreachable or not configured: %s", redactDiagnosticError(err, sessionKey))
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "send last.fm request") ||
		strings.Contains(message, "create last.fm request") ||
		strings.Contains(message, "parse last.fm api url") ||
		strings.Contains(message, "last.fm client is not configured") {
		return fmt.Errorf("Last.fm API is unreachable or not configured: %s", redactDiagnosticError(err, sessionKey))
	}
	return fmt.Errorf("Last.fm API request failed: %s", redactDiagnosticError(err, sessionKey))
}

func checkDoctorJournal(store journal.Store, profile string) error {
	if store == nil {
		var err error
		store, err = journal.NewDefaultStore()
		if err != nil {
			return fmt.Errorf("create journal store: %w", err)
		}
	}
	if readOnly, ok := store.(journal.ReadOnlyStore); ok {
		if err := readOnly.CheckReadable(profile); err != nil {
			return fmt.Errorf("journal is unreadable: %w", err)
		}
		return nil
	}
	if _, err := store.ListRuns(profile); err != nil {
		return fmt.Errorf("journal is unreadable: %w", err)
	}
	return nil
}

func redactDiagnosticError(err error, secret string) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if secret != "" {
		message = strings.ReplaceAll(message, secret, "[redacted]")
	}
	return message
}

func printDoctorResult(w io.Writer, result doctorResult) {
	status, color := "PASS", "32"
	if result.err != nil {
		status, color = "FAIL", "31"
	}
	if doctorColorEnabled(w) {
		status = fmt.Sprintf("\x1b[%sm%s\x1b[0m", color, status)
	}
	if result.err == nil {
		fmt.Fprintf(w, "%s: %s\n", status, result.name)
	} else {
		fmt.Fprintf(w, "%s: %s: %s\n", status, result.name, result.err)
	}
}

func doctorColorEnabled(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("FORCE_COLOR") != "" || os.Getenv("CLICOLOR_FORCE") != "" {
		return true
	}
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func printDoctorDependentFailures(w io.Writer, name, reason string) {
	printDoctorResult(w, doctorResult{name: name, err: errors.New(reason)})
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

func durationLookupWarningHandler(stderr io.Writer) lastfm.EligibilityLookupFailureHandler {
	return func(artist, track string, err error) {
		fmt.Fprintf(stderr, "warning: duration_lookup_failed: Last.fm duration lookup failed for artist %q, track %q: %v\n", artist, track, err)
	}
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
	timestampTolerance, timestampToleranceSet := resolveTimestampTolerance(cfg, options)

	var ingestionSummary spotify.Summary
	exclusionCounts := make(map[string]int)
	progress := observability.NewProgress(stdout)
	summary, err := lastfm.Compare(
		context.Background(),
		dependencies.LastFMClient,
		lastfm.AuthenticatedProfile{Username: profile.LastFMUsername, SessionKey: sessionKey},
		lastfm.ComparisonRequest{
			From:                         from,
			To:                           to,
			Timezone:                     location,
			TimestampTolerance:           timestampTolerance,
			TimestampToleranceSet:        timestampToleranceSet,
			DurationLookupFailureHandler: durationLookupWarningHandler(stderr),
			Plays: func(ctx context.Context, consume spotify.Consumer) error {
				deliveredRecords := 0
				progressingConsume := func(play spotify.Play) error {
					if err := consume(play); err != nil {
						return err
					}
					deliveredRecords++
					if deliveredRecords%1000 == 0 {
						return progress.Update(deliveredRecords, 0, "records ingested")
					}
					return nil
				}
				var ingestErr error
				ingestionSummary, ingestErr = spotify.IngestFiles(ctx, paths, progressingConsume, func(warning spotify.Warning) {
					exclusionCounts[warning.Code]++
				})
				if ingestErr != nil {
					return ingestErr
				}
				return progress.Complete(fmt.Sprintf("progress complete: %d records ingested", deliveredRecords))
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
		fmt.Fprintln(stderr, "usage: rescrobble [options] import [--from YYYY-MM-DD] [--to YYYY-MM-DD] [--timestamp-tolerance duration] [--batch-delay duration] [--dry-run] [--yes] <export>...")
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
	timestampTolerance, timestampToleranceSet := resolveTimestampTolerance(cfg, options)

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
	submissionDelay := resolveSubmissionDelay(cfg, options, dependencies.Submission)
	settings := journal.RunSettings{
		From: from, To: to,
		TimestampTolerance: timestampTolerance, TimestampToleranceSet: true,
		EligibilityRule: "eligible when the play satisfies Last.fm's scrobble eligibility rule",
		BatchDelay:      submissionDelay, BatchDelaySet: true,
	}
	if metadataStore, ok := store.(journal.MetadataStore); ok {
		if err := metadataStore.SetRunSettings(profileName, invocationID, settings); err != nil {
			fmt.Fprintf(stderr, "error: record import settings: %v\n", err)
			return 1
		}
	} else {
		fmt.Fprintln(stderr, "error: journal store does not support portable run settings")
		return 1
	}
	events := []journal.Event{{
		Type: "run.started", Timestamp: time.Now().UTC(),
		Data: map[string]string{"profile": profileName},
	}}
	failureBatch := ""
	recordEvents := func() error {
		if len(events) == 0 {
			return nil
		}
		if recorder, ok := store.(journal.EventBatchRecorder); ok {
			err := recorder.RecordEvents(profileName, invocationID, events)
			if err == nil {
				events = nil
			}
			return err
		}
		recorder, ok := store.(journal.EventRecorder)
		if !ok {
			return errors.New("journal store does not support portable run events")
		}
		for _, event := range events {
			if err := recorder.RecordEvent(profileName, invocationID, event); err != nil {
				return err
			}
		}
		events = nil
		return nil
	}
	failImport := func(importErr error) int {
		data := map[string]string{
			"failure_id": invocationID + ":run",
			"message":    redactImportError(importErr.Error(), sessionKey),
		}
		if failureBatch != "" {
			data["related_batch"] = failureBatch
		}
		events = append(events, journal.Event{
			Type: "run.failed", Timestamp: time.Now().UTC(),
			Data: data,
		})
		if recordErr := recordEvents(); recordErr != nil {
			fmt.Fprintf(stderr, "error: record import outcome: %v (original error: %v)\n", recordErr, importErr)
			return 1
		}
		fmt.Fprintf(stderr, "error: %v\n", importErr)
		return 1
	}

	var ingestionSummary spotify.Summary
	missing := make([]spotify.Play, 0)
	matchedCount := 0
	eligibleCount := 0
	progress := observability.NewProgress(stdout)
	summary, err := lastfm.Compare(
		context.Background(),
		dependencies.LastFMClient,
		lastfm.AuthenticatedProfile{Username: profile.LastFMUsername, SessionKey: sessionKey},
		lastfm.ComparisonRequest{
			From:                         from,
			To:                           to,
			Timezone:                     location,
			TimestampTolerance:           timestampTolerance,
			TimestampToleranceSet:        timestampToleranceSet,
			DurationLookupFailureHandler: durationLookupWarningHandler(stderr),
			Plays: func(ctx context.Context, consume spotify.Consumer) error {
				deliveredRecords := 0
				progressingConsume := func(play spotify.Play) error {
					if err := consume(play); err != nil {
						return err
					}
					deliveredRecords++
					if deliveredRecords%1000 == 0 {
						return progress.Update(deliveredRecords, 0, "records ingested")
					}
					return nil
				}
				warnings := newWarningPrinter(stderr, progress.IsTerminal(), options.logLevel >= observability.LevelVerbose)
				var ingestErr error
				ingestionSummary, ingestErr = spotify.IngestFiles(ctx, paths, progressingConsume, func(warning spotify.Warning) {
					warnings.Print(warning)
					events = append(events, journal.Event{
						Type: "ingestion.warning", Timestamp: time.Now().UTC(),
						Data: warningEventData(warning),
					})
				})
				warningsErr := warnings.Flush()
				// The ingestion error is the root cause; a failure to write
				// warning output must never mask it.
				if ingestErr != nil {
					return ingestErr
				}
				if warningsErr != nil {
					return fmt.Errorf("write ingestion warnings: %w", warningsErr)
				}
				return progress.Complete(fmt.Sprintf("progress complete: %d records ingested", deliveredRecords))
			},
		},
		func(result lastfm.ComparisonResult) error {
			if result.Status == lastfm.ComparisonStatusMissing {
				missing = append(missing, result.Play)
			} else {
				matchedCount++
			}
			eligibleCount++
			return nil
		},
	)
	if err != nil {
		return failImport(fmt.Errorf("import comparison: %w", err))
	}
	events = appendComparisonEvents(events, summary, matchedCount, eligibleCount)

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
		events = append(events, journal.Event{Type: "run.completed", Timestamp: time.Now().UTC()})
		if err := recordEvents(); err != nil {
			fmt.Fprintf(stderr, "error: record import outcome: %v\n", err)
			return 1
		}
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
			return failImport(fmt.Errorf("read import confirmation: %w", readErr))
		}
		answer = strings.TrimSpace(strings.ToLower(answer))
		if answer != "y" && answer != "yes" {
			fmt.Fprintln(stdout, "Import cancelled.")
			events = append(events, journal.Event{
				Type: "run.failed", Timestamp: time.Now().UTC(),
				Data: map[string]string{"failure_id": invocationID + ":cancelled", "message": "import cancelled"},
			})
			if err := recordEvents(); err != nil {
				fmt.Fprintf(stderr, "error: record import outcome: %v\n", err)
				return 1
			}
			return 1
		}
	}

	submissionOptions := dependencies.Submission
	submissionOptions.BaselineDelay = submissionDelay
	submissionOptions.BaselineDelaySet = true
	submissionOptions.Progress = func(completed, total int) error {
		return progress.Update(completed, total, "batches submitted")
	}
	service := lastfm.NewSubmissionService(dependencies.LastFMClient, store, submissionOptions)
	authenticated := lastfm.AuthenticatedProfile{Username: profile.LastFMUsername, SessionKey: sessionKey}
	if options.dryRun {
		if err := service.PlanSpotify(context.Background(), run, missing); err != nil {
			return failImport(fmt.Errorf("plan import batches: %w", err))
		}
		currentRun, openErr := store.OpenRun(profileName, invocationID)
		if openErr != nil {
			return failImport(fmt.Errorf("read planned import journal: %w", openErr))
		}
		events = appendBatchOutcomeEvents(events, currentRun)
		events = append(events, journal.Event{Type: "run.completed", Timestamp: time.Now().UTC()})
		if err := recordEvents(); err != nil {
			fmt.Fprintf(stderr, "error: record import outcome: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "Planned missing plays in the import journal; no batches submitted.")
		return 0
	}
	if err := service.SubmitSpotify(context.Background(), authenticated, run, missing); err != nil {
		currentRun, openErr := store.OpenRun(profileName, invocationID)
		if openErr == nil {
			events = appendBatchOutcomeEvents(events, currentRun)
			for _, batch := range currentRun.Batches {
				if batch.State == journal.StatePlanned {
					failureBatch = strconv.Itoa(batch.Sequence)
					events = append(events, journal.Event{
						Type: "submission.batch.failed", Timestamp: time.Now().UTC(),
						Data: map[string]string{"batch": strconv.Itoa(batch.Sequence), "message": redactImportError(err.Error(), sessionKey)},
					})
					break
				}
			}
		}
		return failImport(fmt.Errorf("submit import batches: %w", err))
	}
	totalBatches := (len(missing) + lastfm.MaxSubmissionBatchSize - 1) / lastfm.MaxSubmissionBatchSize
	if err := progress.Complete(fmt.Sprintf("progress complete: %d batches submitted", totalBatches)); err != nil {
		return failImport(fmt.Errorf("complete import submission progress: %w", err))
	}
	currentRun, openErr := store.OpenRun(profileName, invocationID)
	if openErr != nil {
		return failImport(fmt.Errorf("read completed import journal: %w", openErr))
	}
	events = appendBatchOutcomeEvents(events, currentRun)
	events = append(events, journal.Event{Type: "run.completed", Timestamp: time.Now().UTC()})
	if err := recordEvents(); err != nil {
		fmt.Fprintf(stderr, "error: record import outcome: %v\n", err)
		return 1
	}
	accepted, ignored := submissionOutcomeCounts(currentRun)
	fmt.Fprintf(stdout, "Accepted %d scrobbles; ignored %d.\n", accepted, ignored)
	for _, event := range currentRun.Events {
		if event.Type != "submission.scrobble.ignored" {
			continue
		}
		fmt.Fprintf(stdout, "  Ignored: %s - %s: %s\n",
			event.Data["artist"], event.Data["track"], event.Data["reason"])
	}
	return 0
}

func runReport(command []string, options options, stdout, stderr io.Writer, dependencies Dependencies) int {
	if len(command) > 1 {
		fmt.Fprintln(stderr, "error: report accepts at most one invocation id")
		return 2
	}
	if dependencies.ConfigStore == nil {
		fmt.Fprintln(stderr, "error: configuration store is unavailable")
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
	store := dependencies.JournalStore
	if store == nil {
		store, err = journal.NewDefaultStore()
		if err != nil {
			fmt.Fprintf(stderr, "error: create report journal: %v\n", err)
			return 1
		}
	}
	var run journal.Run
	if len(command) == 1 {
		run, err = store.OpenRun(profileName, command[0])
	} else {
		var runs []journal.Run
		runs, err = store.ListRuns(profileName)
		if len(runs) > 0 {
			run = runs[len(runs)-1]
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "error: read report journal: %v\n", err)
		return 1
	}
	if run.InvocationID == "" {
		fmt.Fprintf(stderr, "error: no journal run found for profile %q\n", profileName)
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
	reportOptions := report.Options{
		Format: options.reportFormat, From: from, To: to, Location: location,
		TimestampTolerance:    options.timestampTolerance,
		TimestampToleranceSet: options.timestampToleranceSet,
		BatchDelay:            options.batchDelay, BatchDelaySet: options.batchDelaySet,
	}
	if !run.Settings.TimestampToleranceSet && run.Settings.TimestampTolerance == 0 &&
		!reportOptions.TimestampToleranceSet {
		reportOptions.TimestampTolerance = cfg.TimestampTolerance
		reportOptions.TimestampToleranceSet = true
	}
	if !run.Settings.BatchDelaySet && run.Settings.BatchDelay == 0 &&
		!reportOptions.BatchDelaySet {
		reportOptions.BatchDelay = cfg.BatchDelay
		reportOptions.BatchDelaySet = true
	}
	if dependencies.CredentialStore != nil {
		if secret, loadErr := dependencies.CredentialStore.Load(profileName); loadErr == nil {
			reportOptions.Secrets = []string{secret}
		}
	}
	data, err := report.Render(run, reportOptions)
	if err != nil {
		fmt.Fprintf(stderr, "error: render report: %v\n", err)
		return 1
	}
	_, err = stdout.Write(data)
	if err != nil {
		fmt.Fprintf(stderr, "error: write report: %v\n", err)
		return 1
	}
	return 0
}

func resolveSubmissionDelay(cfg config.Config, options options, configured lastfm.SubmissionOptions) time.Duration {
	var delay time.Duration
	if options.batchDelaySet {
		delay = options.batchDelay
	} else if configured.BaselineDelaySet || configured.BaselineDelay != 0 {
		delay = configured.BaselineDelay
	} else if cfg.BatchDelay != 0 {
		delay = cfg.BatchDelay
	} else {
		delay = lastfm.DefaultSubmissionDelay
	}
	if delay < 0 {
		return 0
	}
	return delay
}

func warningEventData(warning spotify.Warning) map[string]string {
	data := map[string]string{
		"input": warning.Input, "code": warning.Code,
		"reason": warning.Reason, "severity": warning.Severity,
	}
	if warning.Record != 0 {
		data["record"] = strconv.Itoa(warning.Record)
	}
	if warning.Field != "" {
		data["field"] = warning.Field
	}
	return data
}

func appendComparisonEvents(events []journal.Event, summary lastfm.ComparisonSummary, matched, eligible int) []journal.Event {
	if eligible > 0 {
		events = append(events, journal.Event{
			Type: "comparison.eligible", Timestamp: time.Now().UTC(),
			Data: map[string]string{"count": strconv.Itoa(eligible)},
		})
	}
	if matched > 0 {
		events = append(events, journal.Event{
			Type: "comparison.matched", Timestamp: time.Now().UTC(),
			Data: map[string]string{"count": strconv.Itoa(matched)},
		})
	}
	reasons := make([]string, 0, len(summary.ExcludedByReason))
	for reason := range summary.ExcludedByReason {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	for _, reason := range reasons {
		events = append(events, journal.Event{
			Type: "comparison.excluded", Timestamp: time.Now().UTC(),
			Data: map[string]string{"reason": reason, "count": strconv.Itoa(summary.ExcludedByReason[reason])},
		})
	}
	return events
}

func appendBatchOutcomeEvents(events []journal.Event, run journal.Run) []journal.Event {
	for _, batch := range run.Batches {
		data := map[string]string{
			"batch": strconv.Itoa(batch.Sequence),
			"count": strconv.Itoa(len(batch.Payloads)),
		}
		events = append(events, journal.Event{
			Type: "submission.batch.planned", Timestamp: batch.PlannedAt, Data: data,
		})
		if batch.State == journal.StateSubmitted {
			events = append(events, journal.Event{
				Type: "submission.batch.submitted", Timestamp: submittedTimestamp(batch), Data: data,
			})
		}
	}
	return events
}

func submittedTimestamp(batch journal.Batch) time.Time {
	if batch.SubmittedAt != nil {
		return *batch.SubmittedAt
	}
	return batch.PlannedAt
}

func redactImportError(message, sessionKey string) string {
	if sessionKey == "" {
		return message
	}
	return strings.ReplaceAll(message, sessionKey, "[redacted]")
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
	ignored := make(map[int]map[int]struct{})
	for _, event := range run.Events {
		if event.Type != "submission.scrobble.ignored" {
			continue
		}
		batch, batchErr := strconv.Atoi(event.Data["batch"])
		index, indexErr := strconv.Atoi(event.Data["index"])
		if batchErr != nil || indexErr != nil || batch < 1 || index < 1 {
			continue
		}
		if ignored[batch] == nil {
			ignored[batch] = make(map[int]struct{})
		}
		ignored[batch][index] = struct{}{}
	}
	for _, batch := range run.Batches {
		if batch.State != journal.StateSubmitted {
			continue
		}
		for index, payload := range batch.Payloads {
			if _, wasIgnored := ignored[batch.Sequence][index+1]; !wasIgnored {
				submissions = append(submissions, payload)
			}
		}
	}
	return submissions
}

func submissionOutcomeCounts(run journal.Run) (accepted, ignored int) {
	results := make(map[int]int)
	for _, event := range run.Events {
		if event.Type != "submission.batch.result" {
			continue
		}
		batch, batchErr := strconv.Atoi(event.Data["batch"])
		count, countErr := strconv.Atoi(event.Data["accepted"])
		if batchErr == nil && countErr == nil && batch > 0 && count >= 0 {
			results[batch] = count
		}
	}
	for _, batch := range run.Batches {
		if batch.State != journal.StateSubmitted {
			continue
		}
		if count, hasResult := results[batch.Sequence]; hasResult {
			accepted += count
		} else {
			accepted += len(batch.Payloads)
		}
	}
	for _, event := range run.Events {
		if event.Type == "submission.scrobble.ignored" {
			ignored++
		}
	}
	return accepted, ignored
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

func resolveTimestampTolerance(cfg config.Config, options options) (time.Duration, bool) {
	if options.timestampToleranceSet {
		return options.timestampTolerance, true
	}
	if cfg.TimestampTolerance != 0 {
		return cfg.TimestampTolerance, true
	}
	return lastfm.DefaultTimestampTolerance, true
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
	fmt.Fprintln(w, "  rescrobble [--profile <name>] doctor")
	fmt.Fprintln(w, "  rescrobble [--profile <name>] analyse [--from YYYY-MM-DD] [--to YYYY-MM-DD] [--timestamp-tolerance duration] <export>...")
	fmt.Fprintln(w, "  rescrobble [--profile <name>] import [--from YYYY-MM-DD] [--to YYYY-MM-DD] [--timestamp-tolerance duration] [--batch-delay duration] [--dry-run] [--yes] <export>...")
	fmt.Fprintln(w, "  rescrobble [--profile <name>] verify [<invocation-id>...]")
	fmt.Fprintln(w, "  rescrobble [--profile <name>] report (--json|--csv|--html) [<invocation-id>]")
	fmt.Fprintln(w, "  Native credential storage is preferred. Linux uses an encrypted local fallback only when Secret Service is unavailable.")
	fmt.Fprintln(w, "  The Linux fallback is machine-bound, not portable, and weaker: same-account users or root can derive its key.")
	fmt.Fprintln(w, "  Store changes do not migrate credentials; login saves to the currently selected tier.")
	fmt.Fprintln(w, "  Log in again to establish a credential in a different tier.")
	fmt.Fprintln(w, "  After machine-identity loss, log in again. Logout clears credentials from both Linux tiers.")
	fmt.Fprintf(w, "  import confirms interactively when more than %d missing plays would be submitted; --yes bypasses confirmation.\n", LargeImportConfirmationThreshold)
	fmt.Fprintln(w, "  rescrobble [--profile <name>] profile use <name>")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Global options:")
	fmt.Fprintln(w, "  --profile <name>  select a configured profile for the command")
	fmt.Fprintln(w, "  --log-level <level>  set logging to normal, verbose, or debug")
	fmt.Fprintln(w, "  --verbose         enable verbose logging")
	fmt.Fprintln(w, "  --debug           enable debug logging")
	fmt.Fprintln(w, "  --json/--csv/--html  select exactly one report format")
	fmt.Fprintln(w, "  --help            show this help")
	fmt.Fprintln(w, "  --version         show the application version")
}

// warningPrinter writes ingestion warnings to w. Unless verbose, only the
// first warning of each code is printed as it happens and Flush prints one
// count line per code, so a run with thousands of identical exclusions stays
// readable. On a terminal, the in-place progress line is cleared first so
// warning text never shares a line with it.
type warningPrinter struct {
	w        io.Writer
	terminal bool
	verbose  bool
	counts   map[string]int
	order    []string
	err      error
}

func newWarningPrinter(w io.Writer, terminal, verbose bool) *warningPrinter {
	return &warningPrinter{w: w, terminal: terminal, verbose: verbose, counts: make(map[string]int)}
}

func (p *warningPrinter) Print(warning spotify.Warning) {
	if _, seen := p.counts[warning.Code]; !seen {
		p.order = append(p.order, warning.Code)
	}
	p.counts[warning.Code]++
	if p.verbose || p.counts[warning.Code] == 1 {
		p.writef("warning: %s: %s\n", warning.Code, warning.Reason)
	}
}

// Flush prints the per-code totals for codes that repeated and returns the
// first write error encountered, if any.
func (p *warningPrinter) Flush() error {
	if !p.verbose {
		for _, code := range p.order {
			if p.counts[code] > 1 {
				p.writef("warning: %s: %d records\n", code, p.counts[code])
			}
		}
	}
	return p.err
}

func (p *warningPrinter) writef(format string, args ...any) {
	if p.err != nil {
		return
	}
	line := fmt.Sprintf(format, args...)
	if p.terminal {
		line = "\r\x1b[2K" + line
	}
	_, p.err = io.WriteString(p.w, line)
}
