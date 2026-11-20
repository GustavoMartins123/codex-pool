package routing

// Mode identifies the set of candidates a request can consider.
type Mode uint8

const (
	MatchEligible Mode = iota
	MatchAccountID
	MatchCyberAccess
	MatchModelFamily
	MatchImageFanout
)

// Request carries the eligibility constraints and the routing policy.
// An omitted profile selects the configured default.
type Request struct {
	Mode           Mode
	Provider       string
	Model          string
	Profile        string
	UserID         string
	ConversationID string
	AccountID      string
	Index          int // Image fanout starts at this index in account ID order.
	RequiredPlan   string
	ClientIP       string
	Exclude        map[string]bool
	Trace          bool
}
