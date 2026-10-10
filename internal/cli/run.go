package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
)

// Main renders one version-1 envelope in JSON mode and returns its process exit.
func Main(ctx context.Context, args []string, env map[string]string, in *os.File, out, errOut io.Writer) int {
	o, e := parseOptions(args, env, isTerminal(in))
	selected := e == nil
	var result Result
	if e == nil {
		r := runner{ctx: ctx, in: in, errOut: errOut, env: env}
		result, e = r.run(o)
	}
	if e == nil {
		if o.Output == "json" {
			fmt.Fprintln(out, wire(object{"version": 1, "command": result.Command, "ok": true, "result": result.Value, "warnings": result.Warnings}))
		} else {
			if s, ok := result.Value.(string); ok {
				fmt.Fprintln(out, s)
			} else if result.Command == "plan" {
				m := obj(result.Value)
				target := obj(m["target"])
				project := obj(target["project"])
				fmt.Fprintf(out, "API %s\nProject %s (%s)\n%s", wire(target["api"]), wire(project["id"]), wire(project["display_name"]), semantic(obj(m["plan"])))
			} else {
				fmt.Fprintln(out, wire(result.Value))
			}
			for _, w := range result.Warnings {
				fmt.Fprintln(errOut, wire(w))
			}
		}
		return 0
	}
	exit, code := 1, "validation_failed"
	details := object{}
	var ce *CommandError
	var ae *APIError
	if errors.As(e, &ce) {
		exit, code = ce.Exit, ce.Code
	} else if errors.As(e, &ae) {
		code = ae.Code
		if (ae.Status == 409 || ae.Status == 412) && !ae.Unknown {
			exit = 2
		}
		details = object{"status": ae.Status, "unknown": ae.Unknown, "retry_after": ae.RetryAfter}
	}
	diagnostics := []Diagnostic{}
	var fe *FileError
	var aggregate *fileErrors
	if errors.As(e, &aggregate) {
		for _, v := range aggregate.Errors {
			if errors.As(v, &fe) {
				diagnostics = append(diagnostics, fe.Diagnostic)
			}
		}
	} else if errors.As(e, &fe) {
		diagnostics = append(diagnostics, fe.Diagnostic)
	}
	jsonMode := o.Output == "json"
	if !selected {
		for i, arg := range args {
			if arg == "--output" && i+1 < len(args) && args[i+1] == "json" {
				jsonMode = true
			}
		}
	}
	if jsonMode {
		command := ""
		if selected {
			command = o.Command
			if slices.Contains([]string{"auth", "projects"}, o.Command) {
				command += " " + strings.Join(o.Args, " ")
			}
		}
		fmt.Fprintln(out, wire(object{"version": 1, "command": command, "ok": false, "error": object{"code": code, "message": e.Error(), "details": details}, "diagnostics": diagnostics}))
	} else {
		fmt.Fprintln(errOut, wire(e.Error()))
	}
	return exit
}
