package apicmd

import (
	"fmt"
	"strings"

	"github.com/semistrict/hotbed/internal/apicmd/spec"
)

// RenderCommandReference renders a deterministic Markdown reference of every
// command declared in s (from hotbed.proto's CommandSpec/ParamSpec options).
func RenderCommandReference(s *spec.Schema) string {
	var b strings.Builder
	b.WriteString("<!-- Code generated from proto/hotbed/v1/hotbed.proto via `go generate ./skills`. DO NOT EDIT. -->\n\n")
	b.WriteString("# Proto-Generated CLI Commands\n\n")
	b.WriteString("Every command below is generated directly from `proto/hotbed/v1/hotbed.proto` and also exposed as an MCP tool (`hotbed mcp`).\n")
	b.WriteString("Pass `--json` for the JSON envelope, or `--jq EXPR` to filter the response JSON.\n")

	var currentGroup string
	for _, c := range s.Commands {
		group := c.Words[0]
		if group != currentGroup {
			currentGroup = group
			fmt.Fprintf(&b, "\n## `%s`\n", group)
			if help := strings.TrimSpace(s.Groups[group]); help != "" {
				fmt.Fprintf(&b, "\n%s\n", help)
			}
		}

		// Heading + usage signature
		var usage []string
		usage = append(usage, "hotbed", c.Name())
		for _, p := range c.Params {
			if p.Hidden || !p.Positional {
				continue
			}
			arg := strings.ToUpper(p.Name)
			if p.Field().IsList() {
				arg += "..."
			}
			if (p.Optional || p.Field().IsList()) && !p.Required {
				arg = "[" + arg + "]"
			}
			usage = append(usage, arg)
		}
		fmt.Fprintf(&b, "\n### `%s`\n", strings.Join(usage, " "))
		if help := strings.TrimSpace(c.Help); help != "" {
			fmt.Fprintf(&b, "\n%s\n", help)
		}

		var meta []string
		meta = append(meta, fmt.Sprintf("MCP tool: `%s`", c.Tool()))
		if c.Session {
			meta = append(meta, "uses `-s/--session`")
		}
		if c.Input != nil {
			meta = append(meta, "reads JSON from stdin or `--file`")
		}
		if c.Waits {
			meta = append(meta, "waits (no short deadline)")
		}
		if len(c.Aliases) > 0 {
			meta = append(meta, "also `"+strings.Join(c.Aliases, "`, `")+"`")
		}
		fmt.Fprintf(&b, "\n*%s*\n", strings.Join(meta, " · "))

		var visible []*spec.Param
		for _, p := range c.Params {
			if !p.Hidden && !p.Message() {
				visible = append(visible, p)
			}
		}
		if len(visible) > 0 {
			b.WriteString("\n| Argument / Flag | Env | Help |\n")
			b.WriteString("| --- | --- | --- |\n")
			for _, p := range visible {
				name := "`--" + p.Name + "`"
				if p.Positional {
					name = "`" + strings.ToUpper(p.Name) + "`"
				} else if p.Short != "" {
					name = "`-" + p.Short + "`, `--" + p.Name + "`"
				}
				env := ""
				if varName, ok := strings.CutPrefix(p.Default, "env:"); ok {
					env = "`" + varName + "`"
				}
				help := strings.ReplaceAll(strings.TrimSpace(p.Help), "\n", " ")
				help = strings.ReplaceAll(help, "|", `\|`)
				fmt.Fprintf(&b, "| %s | %s | %s |\n", name, env, help)
				if p.FileFlag != "" {
					fmt.Fprintf(&b, "| `--%s` |  | Read `--%s` from this file (`-` for stdin). |\n", p.FileFlag, p.Name)
				}
			}
		}
	}
	return b.String()
}
