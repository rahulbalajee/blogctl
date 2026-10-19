// Package cli is blogctl's command line: Cobra commands over the Blog API
// client, with text output for people, --output json for agents, and one exit
// code per failure kind so scripts can react without parsing messages.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rahulbalajee/blogctl/internal/client"
)

// Exit codes. Usage and local input errors share 2.
const (
	ExitOK          = 0
	ExitError       = 1
	ExitUsage       = 2
	ExitAuth        = 3
	ExitForbidden   = 4
	ExitNotFound    = 5
	ExitConflict    = 6
	ExitRateLimited = 7
	ExitUnavailable = 8
	ExitBlocked     = 9
	ExitAmbiguous   = 10
)

// Env is everything the command line touches outside its arguments, so tests
// can run it without a terminal, real files or the network.
type Env struct {
	Args    []string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Getenv  func(string) string
	Version string
	// OpenFD opens an inherited file descriptor for --credentials-fd.
	OpenFD func(fd int) (io.ReadCloser, error)
	// HomeDir finds the default credentials file.
	HomeDir func() (string, error)
	// Transport and Sleep replace the network and retry waits in tests.
	Transport http.RoundTripper
	Sleep     func(context.Context, time.Duration) error
}

// usageError is a malformed command line or local input.
type usageError struct {
	err error
}

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

func usagef(format string, args ...any) error {
	return usageError{fmt.Errorf(format, args...)}
}

// app holds the parsed global flags and builds the API client on demand, so
// commands that need no credentials never read them.
type app struct {
	env             Env
	baseURL         string
	credentialsFile string
	credentialsFD   int
	output          string
	timeout         time.Duration
	allowIPv6       bool
	api             *client.Client
}

// Run executes blogctl and returns its exit code.
func Run(ctx context.Context, env Env) int {
	a := &app{env: env}
	root := a.rootCommand()
	root.SetArgs(env.Args)
	root.SetIn(env.Stdin)
	root.SetOut(env.Stdout)
	root.SetErr(env.Stderr)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return ExitOK
	}
	return a.report(err)
}

func (a *app) rootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "blogctl",
		Short: "Draft posts for rahulbalajee.com through its Blog API",
		Long: `blogctl manages an OAuth client's drafts on rahulbalajee.com: list, read,
create and update them, and upload images for them. It cannot publish,
unpublish or delete; the owner does that in the studio.

Credentials come from --credentials-fd, from BLOGCTL_CLIENT_ID and
BLOGCTL_CLIENT_SECRET, or from a credentials file readable only by you
(default ~/.config/blogctl/credentials.json). They are never accepted as
arguments, and tokens stay in memory.

Rate-limited requests wait out the site's Retry-After (at most 60 s, twice)
before exit 7. A rejected token is replaced once; each run exchanges at
least one token, and the site allows 10 exchanges a minute per client.

Exit codes: 0 ok, 1 error, 2 usage or invalid input, 3 credentials missing
or rejected, 4 forbidden, 5 not found, 6 conflict, 7 rate limited,
8 unavailable, 9 blocked at the site's edge, 10 unclear write outcome.`,
		Version:           a.env.Version,
		SilenceUsage:      true,
		SilenceErrors:     true,
		DisableAutoGenTag: true,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			if a.output != "text" && a.output != "json" {
				return usagef("--output must be text or json")
			}
			// Checked before credentials are read, so a wrong origin is a
			// usage error and nothing is loaded for it.
			if err := client.CheckBaseURL(a.baseURL); err != nil {
				return usageError{err}
			}
			return nil
		},
	}
	root.SetVersionTemplate("blogctl {{.Version}}\n")
	// Cobra's own help and completion commands exit 0 on unknown arguments;
	// blogctl's exit codes must stay meaningful, so help validates its topic
	// and shell completion (not needed by agents) is left out.
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetHelpCommand(&cobra.Command{
		Use:   "help [command]",
		Short: "Help about any command",
		RunE: func(cmd *cobra.Command, args []string) error {
			target, rest, err := cmd.Root().Find(args)
			if err != nil || len(rest) > 0 {
				return usagef("unknown help topic %q", strings.Join(args, " "))
			}
			return target.Help()
		},
	})
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return usageError{err}
	})
	flags := root.PersistentFlags()
	flags.StringVar(&a.baseURL, "base-url", a.getenvOr("BLOGCTL_BASE_URL", client.DefaultBaseURL), "site origin (env BLOGCTL_BASE_URL)")
	flags.StringVar(&a.credentialsFile, "credentials-file", a.env.Getenv("BLOGCTL_CREDENTIALS_FILE"), "credentials file (env BLOGCTL_CREDENTIALS_FILE; default ~/.config/blogctl/credentials.json)")
	flags.IntVar(&a.credentialsFD, "credentials-fd", -1, "read credentials JSON from this inherited file descriptor")
	flags.StringVarP(&a.output, "output", "o", a.getenvOr("BLOGCTL_OUTPUT", "text"), "output format: text or json (env BLOGCTL_OUTPUT)")
	flags.DurationVar(&a.timeout, "timeout", 30*time.Second, "timeout for each HTTP request")
	flags.BoolVar(&a.allowIPv6, "ipv6", false, "allow IPv6 (the edge allowlist is IPv4, so this is off by default)")

	root.AddCommand(a.authCommand(), a.postsCommand(), a.mediaCommand(), a.versionCommand())
	return root
}

func (a *app) getenvOr(name, fallback string) string {
	if value := a.env.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// client builds the API client the first time a command needs it.
func (a *app) client(ctx context.Context) (*client.Client, error) {
	if a.api != nil {
		return a.api, nil
	}
	creds, err := a.loadCredentials(ctx)
	if err != nil {
		return nil, err
	}
	api, err := client.New(client.Config{
		BaseURL:      a.baseURL,
		ClientID:     creds.ClientID,
		ClientSecret: creds.ClientSecret,
		UserAgent:    "blogctl/" + a.env.Version,
		Timeout:      a.timeout,
		AllowIPv6:    a.allowIPv6,
		Transport:    a.env.Transport,
		Sleep:        a.env.Sleep,
	})
	if err != nil {
		return nil, err
	}
	a.api = api
	return api, nil
}

func (a *app) versionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the blogctl version",
		Args:  exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.output == "json" {
				return a.printJSON(cmd, map[string]string{"version": a.env.Version})
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "blogctl %s\n", a.env.Version)
			return err
		},
	}
}

// exactArgs is cobra.ExactArgs that reports a usage error.
func exactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := cobra.ExactArgs(n)(cmd, args); err != nil {
			return usageError{err}
		}
		return nil
	}
}

func (a *app) printJSON(cmd *cobra.Command, v any) error {
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(v)
}

// report prints err once and returns its exit code. JSON mode writes a JSON
// object to stderr, so agents can parse failures as well as results.
func (a *app) report(err error) int {
	code, kind := ExitError, "error"
	var usage usageError
	var apiErr *client.Error
	switch {
	case errors.As(err, &usage), cobraUsageError(err):
		code, kind = ExitUsage, "usage"
	case errors.As(err, &apiErr):
		kind = apiErr.Kind.String()
		code = exitCode(apiErr.Kind)
	}
	if a.output == "json" || wantsJSON(a.env.Args) {
		body := map[string]any{"kind": kind, "message": err.Error()}
		if apiErr != nil {
			body["message"] = apiErr.Message
			if apiErr.Status != 0 {
				body["status"] = apiErr.Status
			}
			if apiErr.Code != "" {
				body["code"] = apiErr.Code
			}
			if apiErr.RequestID != "" {
				body["requestId"] = apiErr.RequestID
			}
			if apiErr.RetryAfter > 0 {
				body["retryAfterSeconds"] = int(apiErr.RetryAfter.Round(time.Second).Seconds())
			}
			if apiErr.CurrentRevision > 0 {
				body["currentRevision"] = apiErr.CurrentRevision
			}
		}
		encoder := json.NewEncoder(a.env.Stderr)
		encoder.SetEscapeHTML(false)
		_ = encoder.Encode(map[string]any{"error": body})
		return code
	}
	fmt.Fprintf(a.env.Stderr, "blogctl: %v\n", err)
	if code == ExitUsage {
		fmt.Fprintln(a.env.Stderr, "Run 'blogctl --help' for usage.")
	}
	return code
}

// wantsJSON finds --output json in the arguments when flag parsing failed
// before the flag was reached, so the error is still machine-readable.
func wantsJSON(args []string) bool {
	for i, arg := range args {
		switch {
		case arg == "--output=json", arg == "-o=json", arg == "-ojson":
			return true
		case (arg == "--output" || arg == "-o") && i+1 < len(args) && args[i+1] == "json":
			return true
		}
	}
	return false
}

// cobraUsageError recognizes the parse errors Cobra returns without passing
// them through the flag error hook.
func cobraUsageError(err error) bool {
	for _, prefix := range []string{"unknown command", "required flag", "accepts ", "invalid argument", "unknown flag", "unknown shorthand", "flag needs an argument"} {
		if strings.HasPrefix(err.Error(), prefix) {
			return true
		}
	}
	return false
}

func exitCode(kind client.Kind) int {
	switch kind {
	case client.KindInvalid:
		return ExitUsage
	case client.KindAuth:
		return ExitAuth
	case client.KindForbidden:
		return ExitForbidden
	case client.KindNotFound:
		return ExitNotFound
	case client.KindConflict:
		return ExitConflict
	case client.KindRateLimited:
		return ExitRateLimited
	case client.KindUnavailable:
		return ExitUnavailable
	case client.KindBlocked:
		return ExitBlocked
	case client.KindAmbiguous:
		return ExitAmbiguous
	default:
		return ExitError
	}
}
