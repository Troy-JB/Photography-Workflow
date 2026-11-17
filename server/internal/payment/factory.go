package payment

import (
	"fmt"
	"time"
)

// New builds the configured provider. Only the sandbox adapter ships today:
// WeChat Pay v3 and Alipay RSA2 need merchant onboarding (merchant id, API v3
// key / certificates, vendor sandbox account) and must be verified against the
// vendor test environment before production money flows through them.
//
// Unknown or known-but-unimplemented names fail startup on purpose. Falling back
// to sandbox would be worse than refusing to boot, because sandbox accepts
// forged callbacks and would mis-handle real payments.
func New(provider, secret, merchantID, payBaseURL string, tolerance time.Duration) (Provider, error) {
	switch provider {
	case "", "sandbox":
		return NewSandbox(secret, merchantID, payBaseURL, tolerance)
	case "wechatpay", "wechat", "alipay":
		return nil, fmt.Errorf("payment provider %q is not implemented in this build: add a payment.Provider adapter, verify it end to end against the vendor sandbox, then release it before switching provider", provider)
	default:
		return nil, fmt.Errorf("payment provider %q is unknown (this build supports: sandbox)", provider)
	}
}
