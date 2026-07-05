package channels

import (
	"fmt"
	"strconv"
	"strings"
)

// extraString reads a string-valued key from a config.ChannelConfig.Extra
// map. Returns the fallback when the key is missing or the value is not
// string-coercible. Adapters call this to pull channel-specific knobs
// (e.g. email's imap_host) out of the generic Extra bag without
// bloating the shared ChannelConfig struct.
func extraString(m map[string]any, key, fallback string) string {
	if m == nil {
		return fallback
	}
	v, ok := m[key]
	if !ok || v == nil {
		return fallback
	}
	switch s := v.(type) {
	case string:
		return s
	case fmt.Stringer:
		return s.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}

// extraBool reads a bool-valued key. Accepts true/false, 1/0, and the
// case-insensitive strings "true"/"yes"/"on" as truthy.
func extraBool(m map[string]any, key string, fallback bool) bool {
	if m == nil {
		return fallback
	}
	v, ok := m[key]
	if !ok || v == nil {
		return fallback
	}
	switch b := v.(type) {
	case bool:
		return b
	case string:
		switch strings.ToLower(b) {
		case "true", "yes", "on", "1":
			return true
		case "false", "no", "off", "0":
			return false
		default:
			return fallback
		}
	case int:
		return b != 0
	case int64:
		return b != 0
	case float64:
		return b != 0
	default:
		return fallback
	}
}

// extraInt reads an int-valued key. Falls back when missing or
// non-numeric. String values are parsed via strconv.Atoi.
func extraInt(m map[string]any, key string, fallback int) int {
	if m == nil {
		return fallback
	}
	v, ok := m[key]
	if !ok || v == nil {
		return fallback
	}
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case string:
		if n, err := strconv.Atoi(n); err == nil {
			return n
		}
		return fallback
	default:
		return fallback
	}
}
