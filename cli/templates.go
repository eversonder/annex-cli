package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
)

// template is a template as the API holds it, and as an annex.yml writes it.
type template struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Route        string   `json:"route"`
	Region       string   `json:"region"`
	VCPU         int      `json:"vcpu"`
	MemoryGiB    int      `json:"memory_gib"`
	Setup        []string `json:"setup"`
	SecretGroups []string `json:"secret_groups"`
	Default      bool     `json:"default"`
	AlwaysRun    bool     `json:"always_run"`
}

// templateFile is the file annex start and annex template apply look for.
const templateFile = "annex.yml"

// parseTemplate reads an annex.yml. It is a small, strict part of YAML, enough
// for a template and no more (the command carries no YAML library):
//
//	name: node-dev            # a key and its value; # starts a comment on a line of its own
//	vcpu: 2
//	default: true
//	secret_groups: [github]   # a list on one line,
//	setup:                    # or one item a line,
//	  - npm install -g pnpm
//	  - |                     # and a command over several lines
//	    apt-get update
//	    apt-get install -y ripgrep
//
// Anything else is an error that names the line.
func parseTemplate(src string) (template, error) {
	var t template
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	indent := func(s string) int { return len(s) - len(strings.TrimLeft(s, " ")) }
	unquote := func(s string) string {
		s = strings.TrimSpace(s)
		if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
		return s
	}
	list := "" // the list being read: "setup" or "secret_groups"
	for i := 0; i < len(lines); i++ {
		raw := lines[i]
		line := strings.TrimSpace(raw)
		bad := func(why string) error { return fmt.Errorf("%s line %d: %s", templateFile, i+1, why) }
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "-") && list != "" { // an item of the list above
			item := strings.TrimSpace(line[1:])
			if item == "|" { // a block: the lines indented deeper than the dash, joined
				var block []string
				base := -1
				for i+1 < len(lines) && (strings.TrimSpace(lines[i+1]) == "" || indent(lines[i+1]) > indent(raw)) {
					i++
					if base < 0 && strings.TrimSpace(lines[i]) != "" {
						base = indent(lines[i])
					}
					block = append(block, strings.TrimPrefix(lines[i], strings.Repeat(" ", max(min(base, indent(lines[i])), 0))))
				}
				item = strings.TrimSpace(strings.Join(block, "\n"))
			} else {
				item = unquote(item)
			}
			if list == "setup" {
				t.Setup = append(t.Setup, item)
			} else {
				t.SecretGroups = append(t.SecretGroups, item)
			}
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok || indent(raw) > 0 {
			return t, bad("expected a line like \"name: node-dev\"")
		}
		key, val, list = strings.TrimSpace(key), strings.TrimSpace(val), ""
		if j := strings.Index(val, " #"); j >= 0 && !strings.ContainsAny(val[:1], `"'`) && key != "description" {
			val = strings.TrimSpace(val[:j]) // a comment after a plain value
		}
		number := func() (int, error) {
			n, err := strconv.Atoi(val)
			if err != nil {
				return 0, bad(key + " is a whole number")
			}
			return n, nil
		}
		var err error
		switch key {
		case "name":
			t.Name = unquote(val)
		case "description":
			t.Description = unquote(val)
		case "route":
			t.Route = unquote(val)
		case "region":
			t.Region = unquote(val)
		case "vcpu":
			t.VCPU, err = number()
		case "memory_gib", "memory":
			t.MemoryGiB, err = number()
		case "default":
			if val != "true" && val != "false" {
				return t, bad("default is true or false")
			}
			t.Default = val == "true"
		case "always_run":
			if val != "true" && val != "false" {
				return t, bad("always_run is true or false")
			}
			t.AlwaysRun = val == "true"
		case "setup", "secret_groups":
			switch {
			case val == "":
				list = key
			case strings.HasPrefix(val, "[") && strings.HasSuffix(val, "]"):
				for _, item := range strings.Split(val[1:len(val)-1], ",") {
					if item = unquote(item); item != "" && key == "setup" {
						t.Setup = append(t.Setup, item)
					} else if item != "" {
						t.SecretGroups = append(t.SecretGroups, item)
					}
				}
			default:
				return t, bad(key + " is a list: put each item on its own line, starting with a dash")
			}
		default:
			return t, bad(fmt.Sprintf("%q isn't something a template has: name, description, route, region, vcpu, memory_gib, setup, secret_groups, default, always_run", key))
		}
		if err != nil {
			return t, err
		}
	}
	if t.Name == "" {
		return t, fmt.Errorf("%s needs a name, such as \"name: node-dev\"", templateFile)
	}
	return t, nil
}

// yml writes t the way parseTemplate reads it.
func (t template) yml() string {
	var b strings.Builder
	fmt.Fprintf(&b, "name: %s\n", t.Name)
	if t.Description != "" {
		fmt.Fprintf(&b, "description: %q\n", t.Description)
	}
	for _, kv := range [][2]string{{"route", t.Route}, {"region", t.Region}} {
		if kv[1] != "" {
			fmt.Fprintf(&b, "%s: %s\n", kv[0], kv[1])
		}
	}
	if t.VCPU > 0 {
		fmt.Fprintf(&b, "vcpu: %d\n", t.VCPU)
	}
	if t.MemoryGiB > 0 {
		fmt.Fprintf(&b, "memory_gib: %d\n", t.MemoryGiB)
	}
	if t.Default {
		b.WriteString("default: true\n")
	}
	if t.AlwaysRun {
		b.WriteString("always_run: true\n")
	}
	if len(t.SecretGroups) > 0 {
		fmt.Fprintf(&b, "secret_groups: [%s]\n", strings.Join(t.SecretGroups, ", "))
	}
	if len(t.Setup) > 0 {
		b.WriteString("setup:\n")
		for _, c := range t.Setup {
			if strings.Contains(c, "\n") {
				b.WriteString("  - |\n    " + strings.ReplaceAll(c, "\n", "\n    ") + "\n")
			} else {
				b.WriteString("  - " + c + "\n")
			}
		}
	}
	return b.String()
}

// applyFile saves the template a file describes and answers its name.
func (a *App) applyFile(ctx context.Context, file string) (template, error) {
	var t template
	b, err := os.ReadFile(file)
	if err != nil {
		return t, err
	}
	if t, err = parseTemplate(string(b)); err != nil {
		return t, err
	}
	base, err := a.ws()
	if err != nil {
		return t, err
	}
	return t, a.call(ctx, "POST", base+"/templates", t, nil)
}

// templates is "annex template …": ls (the default), show, apply and rm.
func (a *App) templates(ctx context.Context, args []string) error {
	verb, rest := "ls", args
	if len(args) > 0 {
		verb, rest = args[0], args[1:]
	}
	base, err := a.ws()
	if err != nil {
		return err
	}
	switch verb {
	case "ls", "list":
		var raw json.RawMessage
		if err := a.call(ctx, "GET", base+"/templates", nil, &raw); err != nil {
			return err
		}
		if a.show(raw) {
			return nil
		}
		var out struct{ Templates []template }
		json.Unmarshal(raw, &out)
		if len(out.Templates) == 0 {
			fmt.Fprintf(a.Out, "No templates yet. Write an %s and run annex template apply.\n", templateFile)
			return nil
		}
		w := tabwriter.NewWriter(a.Out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tDEFAULT\tSIZE\tROUTE\tSET-UP")
		for _, t := range out.Templates {
			size, def := "-", ""
			if t.VCPU > 0 || t.MemoryGiB > 0 {
				size = fmt.Sprintf("%d vCPU · %d GiB", t.VCPU, t.MemoryGiB)
			}
			if t.Default {
				def = "yes"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d command(s)\n", t.Name, def, size, cmpOr(t.Route, "-"), len(t.Setup))
		}
		return w.Flush()
	case "show":
		if len(rest) != 1 {
			return errors.New("annex template show <name>")
		}
		var out struct{ Template template }
		var raw json.RawMessage
		if err := a.call(ctx, "GET", base+"/templates/"+rest[0], nil, &raw); err != nil {
			return err
		}
		if a.show(raw) {
			return nil
		}
		json.Unmarshal(raw, &out)
		fmt.Fprint(a.Out, out.Template.yml())
		return nil
	case "apply":
		file := templateFile
		if len(rest) > 0 {
			file = rest[0]
		}
		t, err := a.applyFile(ctx, file)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.Out, "Saved template %s (%d set-up command(s)).", t.Name, len(t.Setup))
		if t.Default {
			fmt.Fprint(a.Out, " Every new sandbox in this workspace now starts from it.")
		}
		fmt.Fprintf(a.Out, "\nannex start --template %s\n", t.Name)
		return nil
	case "from": // a template written from a sandbox, as an annex.yml to prune and save
		id, _, err := a.pick(ctx, rest)
		if err != nil {
			return err
		}
		var raw json.RawMessage
		if err := a.call(ctx, "GET", base+"/sandboxes/"+id+"/template", nil, &raw); err != nil {
			return err
		}
		if a.show(raw) {
			return nil
		}
		var out struct{ Template template }
		json.Unmarshal(raw, &out)
		out.Template.Name = "my-template"
		fmt.Fprintf(a.Out, "# Written from %s: the commands that were run in it, oldest first.\n# Keep the ones that set it up, give it a name, then: annex template apply\n%s", id, out.Template.yml())
		return nil
	case "rm", "delete":
		if len(rest) != 1 {
			return errors.New("annex template rm <name>")
		}
		if err := a.call(ctx, "DELETE", base+"/templates/"+rest[0], nil, nil); err != nil {
			return err
		}
		fmt.Fprintf(a.Out, "Deleted template %s.\n", rest[0])
		return nil
	}
	return fmt.Errorf("annex template %s: no such command. Use ls, show, apply, from or rm.", verb)
}
