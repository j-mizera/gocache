// Package command provides shared types for command handling in GoCache.
//
// This package lives in api/ and has zero dependencies on server internals.
// Both the plugin SDK and server-side code import these types.
package command

// Result holds the return value or error from a command handler.
type Result struct {
	Value any
	Err   error
}

// Spec defines the minimum and maximum number of arguments a command
// accepts (not counting the command name itself). Max == -1 means unlimited.
type Spec struct {
	Min int
	Max int
}
