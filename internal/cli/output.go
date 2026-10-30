package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"unicode"
)

// Keep labels readable without allowing terminal escapes or forged output lines.
func textValue(value string) string {
	var out strings.Builder
	for _, r := range value {
		if unicode.IsControl(r) {
			fmt.Fprintf(&out, "\\u%04x", r)
		} else {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func printText(out io.Writer, result Result) {
	value := obj(result.Value)
	line := func(label string, value any) {
		if value != nil && value != "" {
			fmt.Fprintf(out, "%s: %s\n", label, textValue(fmt.Sprint(value)))
		}
	}
	context := func(target object) {
		line("API", target["api"])
		project := obj(target["project"])
		if project != nil {
			line("Project", str(project["display_name"])+" ("+str(project["id"])+")")
		}
	}
	switch result.Command {
	case "auth login", "auth status":
		line("API", value["api"])
		line("Account", obj(obj(value["identity"])["principal"])["user_id"])
		line("Credential", value["credential_source"])
		if result.Command == "auth login" {
			fmt.Fprintln(out, "Signed in. Use 'aimount link' to select a Project.")
		}
	case "auth logout":
		fmt.Fprintln(out, "Local authorization removed. Server PATs were not revoked.")
		line("API", value["api"])
	case "switch":
		line("API", value["api"])
		organization := obj(value["organization"])
		line("Organization", str(organization["name"])+" ("+str(organization["id"])+")")
	case "projects list":
		line("API", value["api"])
		line("Organization", value["organization_id"])
		items := objects(value["items"])
		if len(items) == 0 {
			fmt.Fprintln(out, "No active Projects found.")
			return
		}
		table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(table, "ID\tNAME\tSTATUS")
		for _, item := range items {
			fmt.Fprintf(table, "%s\t%s\t%s\n", textValue(str(item["id"])), textValue(str(item["display_name"])), textValue(str(item["status"])))
		}
		table.Flush()
	case "unlink":
		fmt.Fprintln(out, "Project binding removed. Remote resources were not changed.")
	case "link", "status":
		context(value)
		line("Account", obj(obj(value["identity"])["principal"])["user_id"])
		line("Organization", value["organization_id"])
		line("Credential", value["credential_source"])
		if result.Command == "link" {
			fmt.Fprintln(out, "Project linked. Use 'aimount pull' to export its Stack.")
		} else if value["project"] == nil {
			fmt.Fprintln(out, "No Project selected. Run 'aimount link --project <id>'.")
		}
	case "plan":
		context(obj(value["target"]))
		fmt.Fprint(out, semantic(obj(value["plan"])))
	case "apply":
		context(obj(value["target"]))
		if value["changed"] == true {
			fmt.Fprintln(out, "Stack applied.")
		} else {
			fmt.Fprintln(out, "Stack is up to date. No changes applied.")
		}
		for _, resource := range objects(value["resources"]) {
			fmt.Fprintf(out, "  %s %s %s\n", textValue(str(resource["operation"])), textValue(str(resource["kind"])), textValue(str(resource["id"])))
		}
	case "pull":
		if value["recovered"] == true {
			fmt.Fprintln(out, "Local pull recovery complete. No remote changes made.")
			return
		}
		context(value)
		files, _ := value["files"].([]string)
		fmt.Fprintf(out, "Pulled %d managed files.\n", len(files))
		for _, file := range files {
			fmt.Fprintln(out, "  "+textValue(file))
		}
	}
}
