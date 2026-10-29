package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// A string option is explicit input: empty and repeated values are not defaults.
type stringOption struct {
	target *string
	set    bool
	replay bool
}

func (v *stringOption) String() string { return *v.target }
func (v *stringOption) Type() string   { return "string" }
func (v *stringOption) Set(value string) error {
	if v.set && !v.replay || value == "" || strings.HasPrefix(value, "--") {
		return errors.New("provide one nonempty value")
	}
	v.set = true
	*v.target = value
	return nil
}

func parseOptions(args []string, env map[string]string, tty bool) (Options, error) {
	o := Options{Root: ".", Output: "text"}
	var output bytes.Buffer
	root := &cobra.Command{
		Use: "aimount", Short: "Manage Aimount App resources as a declarative Stack",
		Long:          "Manage Models, Tools, Agents and Applications in an existing Project.\nUse pull to export configuration, plan to review changes, and apply to save them.",
		Example:       "  aimount auth login\n  aimount link --project prj_0000000000000001 --root ./stack\n  aimount pull --root ./stack\n  aimount plan --root ./stack",
		SilenceErrors: true, SilenceUsage: true,
		SuggestionsMinimumDistance: 2,
		CompletionOptions:          cobra.CompletionOptions{DisableDefaultCmd: true},
	}
	root.SetOut(&output)
	root.SetErr(io.Discard)
	root.SetArgs(append([]string{}, args...))
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		var invalid *pflag.InvalidValueError
		var missing *pflag.ValueRequiredError
		switch {
		case errors.As(err, &invalid):
			return fmt.Errorf("invalid --%s: provide one valid value", invalid.GetFlag().Name)
		case errors.As(err, &missing):
			return fmt.Errorf("--%s requires a value", missing.GetFlag().Name)
		default:
			return errors.New("unsupported option; run the command with --help")
		}
	})
	flags := root.PersistentFlags()
	inputs := map[string]*stringOption{}
	for _, f := range []struct {
		name, usage string
		target      *string
	}{
		{"root", "Stack directory (no parent-directory search)", &o.Root},
		{"api-url", "App API HTTPS URL (or AIMOUNT_API_URL)", &o.API},
		{"project", "Project ID (or AIMOUNT_PROJECT / local binding)", &o.Project},
		{"organization", "Organization ID for switch", &o.Organization},
		{"output", "Output format: text or json", &o.Output},
	} {
		input := &stringOption{target: f.target}
		inputs[f.name] = input
		flags.Var(input, f.name, f.usage)
	}
	flags.BoolVar(&o.NonInteractive, "non-interactive", false, "Never prompt; fail when confirmation is required")
	flags.Bool("no-color", false, "Disable terminal colors (output is currently plain)")
	// Preserve flags placed before the command; only the owning leaf advertises them.
	for _, flag := range []struct {
		name   string
		target *bool
	}{{"prune", &o.Prune}, {"force", &o.Force}, {"auto-approve", &o.Approve}, {"yes", &o.Yes}} {
		flags.BoolVar(flag.target, flag.name, false, "")
		flags.MarkHidden(flag.name)
	}
	noArgs := func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			message := "Unexpected arguments. Run '" + cmd.CommandPath() + " --help' for usage."
			if cmd.HasSubCommands() {
				if suggestions := cmd.SuggestionsFor(args[0]); len(suggestions) > 0 {
					message += " Did you mean: " + strings.Join(suggestions, ", ") + "?"
				}
			}
			if cmd == root {
				return errors.New(message)
			}
			return commandError("command_invalid", message, 1)
		}
		return nil
	}
	leaf := func(parent *cobra.Command, name, short, example string) *cobra.Command {
		cmd := &cobra.Command{Use: name, Short: short, Example: example, Args: noArgs}
		cmd.RunE = func(cmd *cobra.Command, _ []string) error {
			o.Command = name
			if parent != root {
				o.Command, o.Args = parent.Name(), []string{name}
			}
			return nil
		}
		parent.AddCommand(cmd)
		return cmd
	}
	group := func(name, short string) *cobra.Command {
		cmd := &cobra.Command{Use: name, Short: short, Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
		cmd.SuggestionsMinimumDistance = 2
		root.AddCommand(cmd)
		return cmd
	}
	root.Args = noArgs
	root.RunE = func(cmd *cobra.Command, _ []string) error { return cmd.Help() }
	auth := group("auth", "Sign in and manage local App authorization")
	leaf(auth, "login", "Sign in through your browser using Device Flow", "  aimount auth login --api-url https://api.aimount.io")
	leaf(auth, "logout", "Remove local authorization for the selected API", "")
	leaf(auth, "status", "Show current account and credential source", "")
	projects := group("projects", "Discover Projects in the selected Organization")
	leaf(projects, "list", "List active Projects", "  aimount projects list")
	leaf(root, "switch", "Select an Organization", "  aimount switch --organization org_0000000000000001")
	link := leaf(root, "link", "Bind a Stack directory to an existing Project", "  aimount link --project prj_0000000000000001 --root ./stack")
	link.Flags().BoolVar(&o.Yes, "yes", false, "Approve replacing the local Project binding")
	leaf(root, "unlink", "Remove the local Project binding", "")
	leaf(root, "status", "Show API, account and Project context", "")
	plan := leaf(root, "plan", "Review changes for the complete Project Stack", "  aimount plan --root ./stack")
	plan.Flags().BoolVar(&o.Prune, "prune", false, "Allow planning deletion of omitted resources")
	apply := leaf(root, "apply", "Review and apply the complete Project Stack", "  aimount apply --root ./stack\n  aimount apply --root ./stack --auto-approve")
	apply.Flags().BoolVar(&o.Prune, "prune", false, "Allow irreversible deletion of omitted resources and owned data")
	apply.Flags().BoolVar(&o.Approve, "auto-approve", false, "Apply without a confirmation prompt (does not enable prune)")
	pull := leaf(root, "pull", "Export the Project Stack into local YAML files", "  aimount pull --root ./stack")
	pull.Flags().BoolVar(&o.Force, "force", false, "Replace existing managed files; preserve unmanaged files")
	root.SetHelpCommand(&cobra.Command{
		Use: "help [command]", Short: "Help about any command",
		RunE: func(_ *cobra.Command, args []string) error {
			cmd, remaining, err := root.Find(args)
			if err != nil || len(remaining) != 0 {
				return commandError("command_invalid", "Unknown help topic. Run 'aimount --help' for commands.", 1)
			}
			return cmd.Help()
		},
	})
	completion := group("completion", "Generate a shell completion script")
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		cmd := &cobra.Command{Use: shell, Short: "Generate " + shell + " completion", Args: noArgs}
		cmd.RunE = func(_ *cobra.Command, _ []string) error {
			o.Command = "completion"
			switch shell {
			case "bash":
				return root.GenBashCompletionV2(&output, true)
			case "zsh":
				return root.GenZshCompletion(&output)
			case "fish":
				return root.GenFishCompletion(&output, true)
			default:
				return root.GenPowerShellCompletionWithDesc(&output)
			}
		}
		completion.AddCommand(cmd)
	}
	// Cobra's completion errors bypass SetErr and print raw arguments globally.
	// Validate once with pflag before allowing its internal repeat parsing.
	request := &cobra.Command{Use: cobra.ShellCompRequestCmd, Aliases: []string{cobra.ShellCompNoDescRequestCmd}, Hidden: true, DisableFlagParsing: true}
	root.AddCommand(request)
	selected, requestArgs, _ := root.Find(args)
	root.RemoveCommand(request)
	completing := selected == request
	if completing {
		o.Command, o.Output = "completion", "text"
		valid := validCompletion(root, requestArgs)
		o.Output = "text" // Preflight may have consumed the user's --output flag.
		if !valid {
			o.Help = fmt.Sprintf(":%d\n", cobra.ShellCompDirectiveError)
			return o, nil
		}
		for _, input := range inputs {
			input.replay = true
		}
	}
	cmd, err := root.ExecuteC()
	if err != nil {
		return o, err
	}
	if output.Len() > 0 {
		o.Help = output.String()
		if completing {
			o.Output = "text" // Shell protocol, never the user's JSON result format.
		} else if o.Command != "completion" {
			o.Command = "help"
		}
		if o.Output != "text" && o.Output != "json" {
			return o, errors.New("invalid --output")
		}
		return o, nil
	}
	for name, commands := range map[string][]string{
		"prune": {"plan", "apply"}, "force": {"pull"}, "auto-approve": {"apply"}, "yes": {"link"},
	} {
		if flags.Changed(name) || cmd.Flags().Changed(name) {
			allowed := false
			for _, command := range commands {
				allowed = allowed || o.Command == command
			}
			if !allowed {
				return o, fmt.Errorf("--%s is not supported by this command", name)
			}
		}
	}
	explicit := map[string]bool{}
	for name, input := range inputs {
		explicit[name] = input.set
	}
	return resolveOptions(o, explicit, env, tty)
}

func validCompletion(root *cobra.Command, args []string) bool {
	if len(args) == 0 {
		return false
	}
	cmd, remaining, err := root.Find(args[:len(args)-1])
	if err != nil {
		return false
	}
	cmd.InitDefaultHelpFlag()
	cmd.LocalFlags() // Merge inherited flags before looking up a value being completed.
	lookup := func(arg string) *pflag.Flag {
		if strings.HasPrefix(arg, "--") {
			return cmd.Flags().Lookup(strings.TrimPrefix(arg, "--"))
		}
		if arg == "-h" {
			return cmd.Flags().ShorthandLookup("h")
		}
		return nil
	}
	last := args[len(args)-1]
	if strings.HasPrefix(last, "-") {
		if name, _, equal := strings.Cut(last, "="); equal && lookup(name) == nil {
			return false
		}
	} else if len(remaining) > 0 {
		previous := remaining[len(remaining)-1]
		if strings.HasPrefix(previous, "-") && !strings.Contains(previous, "=") && previous != "--" {
			flag := lookup(previous)
			if flag == nil {
				return false
			}
			if flag.NoOptDefVal == "" {
				remaining = remaining[:len(remaining)-1]
			}
		}
	}
	return cmd.ParseFlags(remaining) == nil
}
