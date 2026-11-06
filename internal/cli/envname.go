package cli

import (
	"regexp"
	"strings"

	"github.com/runners-io/cli/internal/platform"
)

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// neverForwarded are the name prefixes --env refuses (upper-cased before the
// comparison). A Scratch machine has no GitHub token, no Actions variables and
// no OIDC token by decision (design D8, D15), and the CLI's own credential must
// not travel to a machine whose job is to run the user's code. Refusing here
// means a script cannot hand a box one of those by accident.
var neverForwarded = []string{"GITHUB_", "ACTIONS_", "RUNNERS_"}

// neverForwardedContaining are name fragments that mark an identity token of
// the OIDC kind whatever the variable is called (design D8: no OIDC in a
// machine). The prefixes above catch the CI system's own; these catch the
// ones a cloud SDK or a script names for itself.
var neverForwardedContaining = []string{"OIDC", "ID_TOKEN"}

// resolveEnv turns the names given to --env into the variables to forward,
// reading each value from the caller's environment. A name that is unset is an
// error rather than an empty value: silently forwarding "" would run the
// command with a variable that is wrong in a way nobody sees. The error names
// the variable and never a value.
func resolveEnv(names []string, lookup func(string) (string, bool)) (map[string]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(names))
	for _, n := range names {
		if !envNameRE.MatchString(n) {
			if strings.Contains(n, "=") {
				// NAME=value: the part after the = is a secret somebody typed
				// on a command line. It must not be repeated in an error.
				return nil, platform.Errorf(platform.KindUsage, "--env takes a variable name only, not NAME=value (that is not a valid variable name): the value is read from your environment")
			}
			return nil, platform.Errorf(platform.KindUsage, "--env %q is not a valid variable name", n)
		}
		up := strings.ToUpper(n)
		for _, p := range neverForwarded {
			if strings.HasPrefix(up, p) {
				return nil, platform.Errorf(platform.KindUsage, "--env %s: variables starting %s are never forwarded to a Scratch machine", n, p)
			}
		}
		for _, frag := range neverForwardedContaining {
			if strings.Contains(up, frag) {
				return nil, platform.Errorf(platform.KindUsage, "--env %s: variables carrying an identity token (%s) are never forwarded to a Scratch machine", n, frag)
			}
		}
		v, ok := lookup(n)
		if !ok {
			return nil, platform.Errorf(platform.KindUsage, "--env %s: the variable is not set in this environment", n)
		}
		out[n] = v
	}
	return out, nil
}
