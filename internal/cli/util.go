package cli

import "strings"

// replacer returns the strings.Replacer used to map Viper config keys
// to environment variable names. Viper's SetEnvKeyReplacer accepts a
// *strings.Replacer.
func replacer() *strings.Replacer {
	return strings.NewReplacer(".", "_", "-", "_")
}
