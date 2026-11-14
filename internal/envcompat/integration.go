package envcompat

import (
	"log/slog"
	"os"
	"strings"
)

// IntegrationTokenEnv names the one secret Cloud and the Engine share. Cloud
// sends it to the Engine (management API, channel credentials, runtime reload)
// and the Engine sends it to Cloud (pack downloads, sign-in handoff, AI gateway,
// analytics events). Set ZANA_INTEGRATION_TOKEN; it is promoted to this name.
const IntegrationTokenEnv = "KORA_INTEGRATION_TOKEN"

// retiredIntegrationTokenEnvs were separate secrets before IntegrationTokenEnv
// replaced them. They are no longer read.
var retiredIntegrationTokenEnvs = []string{
	"KORA_ENGINE_MANAGEMENT_TOKEN",
	"KORA_ENGINE_MANAGEMENT_ARTIFACT_TOKEN",
	"KORA_ENGINE_PROVISIONING_TOKEN",
	"KORA_ENGINE_CLOUD_IDENTITY_TOKEN",
	"KORA_RELOAD_TOKEN",
	"KORA_CLOUD_AI_GATEWAY_TOKEN",
	"KORA_CLOUD_EVENTS_TOKEN",
}

// IntegrationToken returns the shared Cloud and Engine secret, or "".
func IntegrationToken() string {
	return strings.TrimSpace(os.Getenv(IntegrationTokenEnv))
}

// retiredTokensSet lists the retired token names that are still set, so the
// operator can remove them. It never returns values.
func retiredTokensSet(lookup func(string) string) []string {
	var set []string
	for _, name := range retiredIntegrationTokenEnvs {
		if strings.TrimSpace(lookup(name)) != "" {
			set = append(set, name)
		}
	}
	return set
}

func warnRetiredTokens(log *slog.Logger, lookup func(string) string) {
	if set := retiredTokensSet(lookup); len(set) > 0 && log != nil {
		log.Warn("these token variables are no longer read; set "+IntegrationTokenEnv+" (or ZANA_INTEGRATION_TOKEN) once instead and remove them", "variables", set)
	}
}
