package cli

import (
	"regexp"
	"strings"

	"github.com/db-branch/db-branch/internal/domain"
)

// engineShape is the form of an engine name (domain's rule); versionShape is
// loose on purpose: each engine has its own version pattern.
var (
	engineShape  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	versionShape = regexp.MustCompile(`^[0-9][A-Za-z0-9._+-]{0,63}$`)
)

// parseRuntimeArg reads "engine@version" given on the command line. The CLI
// does not load runtime modules, so an engine it does not know is not an error
// here: the agent, which has the modules, decides. A built-in engine and
// anything that cannot be an engine@version are checked as before.
func parseRuntimeArg(s string) (domain.RuntimeRef, error) {
	ref, err := domain.ParseRuntimeRef(s)
	if err == nil {
		return ref, nil
	}
	engine, version, found := strings.Cut(s, "@")
	if found && engineShape.MatchString(engine) && versionShape.MatchString(version) {
		if _, known := domain.DefaultRuntimes.Spec(domain.Engine(engine)); !known {
			return domain.RuntimeRef{Engine: domain.Engine(engine), Version: version}, nil
		}
	}
	return domain.RuntimeRef{}, err
}
