// Package wizard builds the `ov init` configuration wizard using the
// charmbracelet/huh form library. The wizard collects server URL,
// account name, OAuth client_credentials, and output preferences,
// then invokes a caller-supplied save function.
//
// To avoid an import cycle with the parent cli package, wizard does
// not import cli. The cli package supplies a SaveFunc that persists
// the resulting config to disk.
package wizard

import (
	"fmt"
	"io"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
)

// Defaults exposed so the cli package can reuse them when wiring the
// save callback. Kept here to keep wizard self-contained.
const (
	DefaultBaseURL = "http://127.0.0.1:8000"
	DefaultFormat  = "table"
)

// Config captures the values the wizard collects. The cli package
// translates this into a CLIConfig before persisting.
type Config struct {
	BaseURL      string
	Account      string
	ClientID     string
	ClientSecret string
	Locale       string
	Format       string
}

// SaveFunc persists a wizard Config. The cli package supplies an
// implementation that writes ~/.ov/config.yaml.
type SaveFunc func(cfg Config) (path string, err error)

// InitCmd returns the `ov init` cobra command. It runs an interactive
// form when stdout is a TTY; in CI / non-interactive contexts the form
// falls back to default values so the command can be scripted.
func InitCmd(out, errw io.Writer, save SaveFunc) *cobra.Command {
	var (
		baseURL      string
		accountName  string
		clientID     string
		clientSecret string
		locale       string
		format       string
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Run the configuration wizard",
		Long: `Run an interactive form to populate ~/.ov/config.yaml.

The wizard collects:
  * server.base_url (default http://127.0.0.1:8000)
  * account.name    (default "default")
  * auth.client_id / auth.client_secret (for client_credentials grant)
  * output.locale   (e.g. en, zh-CN)
  * output.format   (table | json)`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// If the user passed --non-interactive, skip the form and
			// persist defaults.
			if nonInteractive, _ := cmd.Flags().GetBool("non-interactive"); nonInteractive {
				return saveAndReport(out, save, baseURL, accountName, clientID, clientSecret, locale, format)
			}
			form := huh.NewForm(
				huh.NewGroup(
					huh.NewInput().Title("OpenViking server URL").Value(&baseURL).Placeholder(DefaultBaseURL),
					huh.NewInput().Title("Account name").Value(&accountName).Placeholder("default"),
				),
				huh.NewGroup(
					huh.NewInput().Title("OAuth client_id").Value(&clientID),
					huh.NewInput().Title("OAuth client_secret").Value(&clientSecret).EchoMode(huh.EchoModePassword),
				),
				huh.NewGroup(
					huh.NewInput().Title("Output locale (e.g. en, zh-CN)").Value(&locale).Placeholder("en"),
					huh.NewSelect[string]().Title("Output format").Value(&format).
						Options(huh.NewOption("table", "table"), huh.NewOption("json", "json")),
				),
			)
			if err := form.Run(); err != nil {
				return fmt.Errorf("wizard: %w", err)
			}
			return saveAndReport(out, save, baseURL, accountName, clientID, clientSecret, locale, format)
		},
	}
	cmd.Flags().Bool("non-interactive", false, "skip the form and write defaults")
	_ = errw
	return cmd
}

func saveAndReport(out io.Writer, save SaveFunc, baseURL, account, clientID, clientSecret, locale, format string) error {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if account == "" {
		account = "default"
	}
	if format == "" {
		format = DefaultFormat
	}
	path, err := save(Config{
		BaseURL:      baseURL,
		Account:      account,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Locale:       locale,
		Format:       format,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote %s\n", path)
	return nil
}
