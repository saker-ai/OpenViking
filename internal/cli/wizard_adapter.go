package cli

import "github.com/saker-ai/ctxhub/internal/cli/wizard"

// saveWizardConfig adapts the wizard.Config shape into a CLIConfig and
// persists it via SaveConfig. It is passed to wizard.InitCmd so the
// wizard package does not need to import the cli package.
func saveWizardConfig(cfg wizard.Config) (string, error) {
	cc := &CLIConfig{
		Server:  CLIConfigServer{BaseURL: cfg.BaseURL, Timeout: 30},
		Account: CLIConfigAccount{Name: cfg.Account},
		Auth: CLIConfigAuth{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Scopes:       []string{"fosite", "openviking"},
		},
		Output:   CLIConfigOutput{Format: cfg.Format, Locale: cfg.Locale},
		Defaults: CLIConfigDefaults{SearchLimit: 10, ListLimit: 50},
	}
	if err := SaveConfig(cc); err != nil {
		return "", err
	}
	path, _ := ConfigPath()
	return path, nil
}
