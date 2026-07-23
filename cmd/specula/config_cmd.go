package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/ivanzzeth/specula/internal/config"
)

// runConfig implements: specula config <apply-example|…>
func runConfig(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, configUsage)
		return fmt.Errorf("missing config subcommand")
	}
	switch args[0] {
	case "apply-example":
		return runConfigApplyExample(args[1:])
	case "help", "-h", "--help":
		fmt.Print(configUsage)
		return nil
	default:
		return fmt.Errorf("unknown config command %q\n%s", args[0], configUsage)
	}
}

const configUsage = `Usage:
  specula config apply-example [flags]

Merge the embedded reference config (specula.example.yaml) into an existing
operator config. This is opt-in: upgrades never rewrite your YAML automatically.

Default merge policy (safe for upgrades):
  • missing keys are copied from the example
  • your existing values win on conflicts
  • string lists are unioned (e.g. git allowed_upstreams)
  • lists of {name: …} maps are merged by name (e.g. apt.repositories)
  • comments on existing keys are preserved

Flags:
  --config PATH     config file (default: specula.yaml)
  --section LIST    only merge protocols.<name> (comma-separated: apt,helm,…)
  --dry-run         show what would change; do not write
  --fill-empty      also replace empty strings/lists/maps from the example
  --overwrite       prefer example values on conflicts (destructive)
  --no-backup       do not write path.bak.<timestamp> before replace

Examples:
  specula config apply-example --dry-run
  specula config apply-example --section apt,helm,conda
  specula config apply-example --config /etc/specula/specula.yaml
  specula config apply-example --fill-empty
`

func runConfigApplyExample(args []string) error {
	fs := flag.NewFlagSet("config apply-example", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, configUsage) }
	configPath := fs.String("config", "specula.yaml", "path to the Specula config file")
	section := fs.String("section", "", "comma-separated protocol names to merge (empty = all)")
	dryRun := fs.Bool("dry-run", false, "show merge result without writing")
	fillEmpty := fs.Bool("fill-empty", false, "replace empty values from the example")
	overwrite := fs.Bool("overwrite", false, "prefer example values on conflicts")
	noBackup := fs.Bool("no-backup", false, "skip writing a .bak timestamped copy")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}

	var sections []string
	if strings.TrimSpace(*section) != "" {
		for _, p := range strings.Split(*section, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				sections = append(sections, p)
			}
		}
	}

	res, err := config.ApplyExample(*configPath, config.ApplyExampleOptions{
		DryRun:    *dryRun,
		FillEmpty: *fillEmpty,
		Overwrite: *overwrite,
		Backup:    true,
		NoBackup:  *noBackup,
		Sections:  sections,
	})
	if err != nil {
		return err
	}
	fmt.Print(config.FormatApplyExampleReport(res))
	if *dryRun {
		fmt.Fprintln(os.Stderr, "hint: re-run without --dry-run to write; restart Specula to load the new config")
	} else if res.Wrote {
		fmt.Fprintln(os.Stderr, "hint: restart Specula to load the merged config")
	}
	return nil
}
