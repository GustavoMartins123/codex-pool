// Package account holds the provider-kind and plan vocabulary shared by the
// pool, pricing and usage tracking.
package account

import "strings"

// AccountType distinguishes between different API backends.
type AccountType string

const (
	AccountTypeCodex       AccountType = "codex"
	AccountTypeGemini      AccountType = "gemini"
	AccountTypeAntigravity AccountType = "antigravity"
	AccountTypeClaude      AccountType = "claude"
	AccountTypeKimi        AccountType = "kimi"
	AccountTypeMinimax     AccountType = "minimax"
	AccountTypeZAI         AccountType = "zai"
	AccountTypeXiaomi      AccountType = "xiaomi"
	AccountTypeGrok        AccountType = "grok"
	AccountTypeAdverserial AccountType = "adverserial"
	AccountTypeOpencodeGo  AccountType = "opencode_go"
	AccountTypeQwen        AccountType = "qwen"
	AccountTypeAIStudio    AccountType = "aistudio"
)

// NormalizeCodexPlanType maps Codex plan variants to their canonical form.
// Business/self-serve variants such as "self_serve_business_prolite" carry
// the same quota as Pro Lite, so they normalize to "prolite". Matching is
// substring-based so future "business_prolite" variants work without a new
// special case.
func NormalizeCodexPlanType(planType string) string {
	p := strings.ToLower(strings.TrimSpace(planType))
	if strings.Contains(p, "prolite") {
		return "prolite"
	}
	return p
}
