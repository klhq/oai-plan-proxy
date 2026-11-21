// Package auth holds the ChatGPT sign-in state and its OAuth tokens.
package auth

// State says whether the proxy holds a usable sign-in.
type State string

const (
	// StateMissing means no credentials are stored.
	StateMissing State = "missing"
	// StateValid means credentials exist and the last refresh did not fail
	// permanently.
	StateValid State = "valid"
	// StateDead means the refresh token was rejected and the operator must
	// sign in again.
	StateDead State = "dead"
)
