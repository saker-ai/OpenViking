// Package i18n provides internationalization for the OpenViking CLI.
//
// It wraps nicksnyder/go-i18n/v2 to load bundled message catalogs and
// translate messages into the user's locale. The default catalog covers
// the English (default) and Simplified Chinese (zh-CN) locales used by
// the CLI's interactive prompts and error output.
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/nicksnyder/go-i18n/v2/i18n"
	"golang.org/x/text/language"
	"golang.org/x/text/language/display"
)

//go:embed locales/*.json
var localeFS embed.FS

// Bundle is the package-level message bundle loaded at init time.
// Tests and callers may call Load to reload or replace it.
var Bundle *i18n.Bundle

// DefaultLocale is used when no preference is expressed via arg or env.
const DefaultLocale = "en"

func init() {
	if err := Load(DefaultLocale); err != nil {
		// Fall back to an empty bundle so callers degrade gracefully.
		Bundle = i18n.NewBundle(language.English)
	}
}

// Load initializes Bundle by parsing the embedded locale JSON files for
// the requested default locale. It is safe to call multiple times.
func Load(defaultLocale string) error {
	tag := parseLocale(defaultLocale)
	b := i18n.NewBundle(tag)
	b.RegisterUnmarshalFunc("json", json.Unmarshal)
	entries, err := localeFS.ReadDir("locales")
	if err != nil {
		return fmt.Errorf("i18n: read locales: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := localeFS.ReadFile("locales/" + e.Name())
		if err != nil {
			return fmt.Errorf("i18n: read %s: %w", e.Name(), err)
		}
		if _, err := b.ParseMessageFileBytes(data, e.Name()); err != nil {
			// Skip files that fail to parse so one bad locale does
			// not break the entire CLI.
			continue
		}
	}
	Bundle = b
	return nil
}

// Localizer returns a Localizer configured for the requested locale.
// If locale is empty, the LC_ALL/LANG env vars are consulted.
func Localizer(locale string) *i18n.Localizer {
	tag := parseLocale(locale)
	if tag == language.Und {
		tag = detectLocaleFromEnv()
	}
	return i18n.NewLocalizer(Bundle, tag.String())
}

// Translate renders the message identified by id in the given locale.
// Template data may be supplied via the data arg. If the id is missing
// the id itself is returned so callers never panic on missing keys.
func Translate(locale, id string, data ...map[string]interface{}) string {
	l := Localizer(locale)
	cfg := &i18n.LocalizeConfig{MessageID: id}
	if len(data) > 0 {
		cfg.TemplateData = data[0]
	}
	msg, err := l.Localize(cfg)
	if err != nil || msg == "" {
		return id
	}
	return msg
}

// Supported returns the locale tags shipped in the bundle.
func Supported() []language.Tag {
	tags := []language.Tag{language.English}
	if Bundle == nil {
		return tags
	}
	for _, tag := range Bundle.LanguageTags() {
		if tag != language.English {
			tags = append(tags, tag)
		}
	}
	return tags
}

// DisplayName returns a human-readable name for tag in the current locale.
func DisplayName(locale string, tag language.Tag) string {
	return display.Tags(parseLocale(locale)).Name(tag)
}

// parseLocale converts a locale string like "zh-CN" or "en_US" to a Tag.
// Empty input yields language.Und so the caller can apply a fallback.
func parseLocale(locale string) language.Tag {
	if locale == "" {
		return language.Und
	}
	// Strip encoding suffix from LC_* env values like "zh_CN.UTF-8".
	if i := strings.IndexByte(locale, '.'); i >= 0 {
		locale = locale[:i]
	}
	locale = strings.ReplaceAll(locale, "_", "-")
	tag, err := language.Parse(locale)
	if err != nil {
		return language.Und
	}
	return tag
}

// detectLocaleFromEnv inspects LC_ALL/LANG and returns the first match.
// Returns DefaultLocale when nothing usable is found.
func detectLocaleFromEnv() language.Tag {
	for _, env := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		v := os.Getenv(env)
		if v == "" || strings.HasPrefix(v, "C") {
			continue
		}
		if tag := parseLocale(v); tag != language.Und {
			return tag
		}
	}
	return language.English
}
