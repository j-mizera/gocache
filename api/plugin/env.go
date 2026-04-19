// Package plugin holds contract constants for the server/plugin boundary:
// environment variables, phase strings, and other shared identifiers that
// both the server-side manager and plugin-author SDK must agree on.
package plugin

// EnvSocketPath is the environment variable name via which the server
// communicates the Unix domain socket path to each plugin process. The
// manager sets it via cmd.Env; the SDK reads it with os.Getenv at startup.
const EnvSocketPath = "GOCACHE_PLUGIN_SOCK"
