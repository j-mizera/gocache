// Package permissions re-exports scope types from api/scope and provides
// the server-side Registry and Enforcer.
package permissions

import "gocache/api/scope"

// Re-export scope types and constants from api/scope.
type Scope = scope.Scope
type OpType = scope.OpType

const (
	ScopeRead     = scope.ScopeRead
	ScopeWrite    = scope.ScopeWrite
	ScopeAdmin    = scope.ScopeAdmin
	ScopeHookPre  = scope.ScopeHookPre
	ScopeHookPost = scope.ScopeHookPost

	ScopeServerQuery        = scope.ScopeServerQuery
	ScopeServerQueryHealth  = scope.ScopeServerQueryHealth
	ScopeServerQueryPlugins = scope.ScopeServerQueryPlugins
	ScopeServerQueryStats   = scope.ScopeServerQueryStats
	ScopeEvents             = scope.ScopeEvents
	ScopeOperationHook      = scope.ScopeOperationHook

	OpRead  = scope.OpRead
	OpWrite = scope.OpWrite
	OpAdmin = scope.OpAdmin
)

// Re-export functions from api/scope.
var (
	IsKeyScope      = scope.IsKeyScope
	KeyPattern      = scope.KeyPattern
	ParseScope      = scope.ParseScope
	ParseScopes     = scope.ParseScopes
	Implies         = scope.Implies
	ValidateRequest = scope.ValidateRequest
	MatchesKey      = scope.MatchesKey
	DefaultScopes   = scope.DefaultScopes
	ScopeStrings    = scope.ScopeStrings
	RequiredScope   = scope.RequiredScope
	ScopeForTopic   = scope.ScopeForTopic
)
