package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/hekmon/rat/mtls"
	"github.com/urfave/cli/v3"
)

func bundleCommand() *cli.Command {
	return &cli.Command{
		Name:  "bundle",
		Usage: "generate and inspect the certificate bundle of a tenant",
		Commands: []*cli.Command{
			{
				Name:  "generate",
				Usage: "generate the bundle of a tenant: a CA, a certificate for ratd, one per client",
				Description: "Each certificate names its side: the server one the tenant, each client one its " +
					"session. The CA key is never written: adding a client, or replacing a key, is generating " +
					"a new bundle. The output directory must not exist.",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "tenant",
						Aliases:  []string{"t"},
						Usage:    "the tenant ratd serves with the bundle",
						Required: true,
					},
					&cli.StringSliceFlag{
						Name:     "client",
						Aliases:  []string{"c"},
						Usage:    "a client of the tenant, which is its session (repeat for each client)",
						Required: true,
					},
					&cli.StringFlag{
						Name:     "output",
						Aliases:  []string{"o"},
						Usage:    "the directory to write the bundle to, which must not exist",
						Required: true,
					},
				},
				Action: generateBundle,
			},
			{
				Name:  "inspect",
				Usage: "check bundle directories as ratd and rat do, and whether they belong to the same bundle",
				// Min 0: the error of the library for a missing argument reads badly
				Arguments: []cli.Argument{
					&cli.StringArgs{Name: "DIR", Min: 0, Max: -1, UsageText: "DIR [DIR ...]"},
				},
				Action: inspectBundle,
			},
		},
	}
}

// generateBundle generates a bundle, and tells what goes where.
func generateBundle(_ context.Context, cmd *cli.Command) error {
	dir := cmd.String("output")
	bundle, err := mtls.Generate(dir, cmd.String("tenant"), cmd.StringSlice("client"))
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%w: a bundle is never written over an existing directory, as the clients "+
			"deployed with a bundle it replaced would stop working: remove it, or choose another one", err)
	}
	if err != nil {
		return err
	}
	out := cmd.Root().Writer
	fmt.Fprintf(out, "Bundle of tenant %s written to %s:\n", bundle.Tenant, dir)
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "  %s\tratd, serving tenant %s\n", mtls.ServerDir(dir), bundle.Tenant)
	for _, client := range bundle.Clients {
		fmt.Fprintf(w, "  %s\tclient %s, working in session %s\n", mtls.ClientDir(dir, client), client, client)
	}
	_ = w.Flush()
	fmt.Fprintf(out, "CA fingerprint %s\n", bundle.CAFingerprint)
	fmt.Fprintf(out, "Valid until %s\n", validity(bundle.NotAfter))
	// Mixing the files of two bundles, or of two sides, is the likeliest mistake.
	fmt.Fprintln(out, "Copy each directory as a whole to its side only: the key of a client grants a shell.")
	return nil
}

// inspectBundle checks each directory, then whether they belong to the same bundle. It fails if a
// directory fails a check, or if they belong to several bundles: checking that directories go
// together is what inspecting several is for.
func inspectBundle(_ context.Context, cmd *cli.Command) error {
	out := cmd.Root().Writer
	dirs := cmd.StringArgs("DIR")
	if len(dirs) == 0 {
		return errors.New("no directory to inspect: give the directory of a side of a bundle, or several")
	}
	failed := 0
	// the directories of each bundle, by CA fingerprint, in the order met
	var fingerprints []string
	bundles := make(map[string][]string)
	for i, dir := range dirs {
		if i > 0 {
			fmt.Fprintln(out)
		}
		fmt.Fprintln(out, dir)
		side := inspectSide(out, dir)
		if side == nil {
			failed++
			continue
		}
		fingerprint := mtls.Fingerprint(side.CA)
		if _, met := bundles[fingerprint]; !met {
			fingerprints = append(fingerprints, fingerprint)
		}
		bundles[fingerprint] = append(bundles[fingerprint], dir)
	}
	switch {
	case len(bundles) > 1:
		fmt.Fprintf(out, "\nThese directories belong to %d different bundles:\n", len(bundles))
		for _, fingerprint := range fingerprints {
			fmt.Fprintf(out, "  CA %s: %s\n", fingerprint, strings.Join(bundles[fingerprint], ", "))
		}
	case len(dirs)-failed > 1:
		fmt.Fprintf(out, "\nThese %d directories belong to the same bundle.\n", len(dirs)-failed)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d directories failed their checks", failed, len(dirs))
	}
	if len(bundles) > 1 {
		return fmt.Errorf("the directories belong to %d different bundles", len(bundles))
	}
	return nil
}

// inspectSide loads dir as ratd and rat do, printing one line per check until one fails, then what
// the directory is for, below a title the caller printed. It returns the side, or nil if a check
// failed.
func inspectSide(out io.Writer, dir string) *mtls.Side {
	side, err := mtls.Load(dir)
	checks := mtls.Checks()
	// the checks before the failing one passed, all of them on success
	passed := len(checks)
	if err != nil {
		passed = slices.IndexFunc(checks, func(check mtls.Check) bool { return errors.Is(err, check.Err) })
	}
	for _, check := range checks[:max(passed, 0)] {
		fmt.Fprintf(out, "  ok    %s\n", check.Name)
	}
	if err != nil {
		if passed >= 0 {
			fmt.Fprintf(out, "  FAIL  %s: %v\n", checks[passed].Name, err)
		} else {
			// matching no check: Load does not fail so (guarded by its tests), but the error is shown
			fmt.Fprintf(out, "  FAIL  %v\n", err)
		}
		return nil
	}
	if side.Role == mtls.Server {
		fmt.Fprintf(out, "  ratd, serving tenant %s\n", side.Tenant)
	} else {
		fmt.Fprintf(out, "  client %s of tenant %s, working in session %s\n", side.Name, side.Tenant, side.Name)
	}
	fmt.Fprintf(out, "  CA fingerprint %s\n", mtls.Fingerprint(side.CA))
	fmt.Fprintf(out, "  valid until %s\n", validity(side.NotAfter()))
	if time.Until(side.NotAfter()) < mtls.ExpiryWarning {
		fmt.Fprintln(out, "  WARNING: less than a year left: an expired bundle stops every client at once, "+
			"generate a new one and deploy it to every side")
	}
	return side
}

// validity tells until when a bundle is valid, and how many days are left.
func validity(notAfter time.Time) string {
	return fmt.Sprintf("%s (%d days left)", notAfter.Local().Format(time.DateOnly), int(time.Until(notAfter).Hours()/24))
}
