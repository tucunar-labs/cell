package canarynode

import (
	"os"
	"strings"
)

// buildVersion is replaced by the release build with -ldflags. Keeping a
// useful default makes local binaries and health reports self-describing.
var buildVersion = "dev"

func currentNodeVersion() string {
	if configured := strings.TrimSpace(os.Getenv("CANARY_NODE_VERSION")); configured != "" {
		return configured
	}
	return buildVersion
}
