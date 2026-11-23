package worker

import (
	"regexp"

	"github.com/semistrict/hotbed/internal/api"
)

// validIDRe permits safe single-component identifiers for workspaces and
// commands: ASCII alphanumeric start, followed by up to 127 ASCII letters,
// digits, hyphens, or underscores (no empty string, slashes, dots,
// whitespace, control characters, or shell metacharacters).
var validIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

func validateID(id string) error {
	if !validIDRe.MatchString(id) {
		return api.Errorf(api.CodeInvalidArgs, "invalid ID %q: must start with an ASCII letter or digit and contain only letters, digits, '-' or '_'", id)
	}
	return nil
}
