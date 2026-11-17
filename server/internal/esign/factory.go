package esign

import (
	"fmt"
	"time"
)

// New builds the configured provider. Only the sandbox adapter ships today: a
// real vendor (for example e签宝) needs an account, app credentials and a
// rendered PDF document, and must be verified against the vendor test
// environment before contracts are sent through it.
//
// The name is validated here rather than defaulted so a typo cannot silently
// route production contracts to the sandbox adapter, which trusts locally
// signed callbacks.
func New(provider, secret, signBaseURL string, tolerance time.Duration) (Provider, error) {
	switch provider {
	case "", "sandbox":
		return NewSandbox(secret, signBaseURL, tolerance)
	case "esignbao", "esign", "docusign":
		return nil, fmt.Errorf("esign provider %q is not implemented in this build: add an esign.Provider adapter (idempotent CreateSignTask, TaskCanceller, verified callbacks) and validate it against the vendor sandbox before enabling it", provider)
	default:
		return nil, fmt.Errorf("esign provider %q is unknown (this build supports: sandbox)", provider)
	}
}
