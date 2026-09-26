// Package config resolves runtime configuration for the Influence server.
//
// Layered resolution applies the fixed precedence CLI flag > env var > config
// file > built-in default to every setting (see Resolve). The fail-fast startup
// validation of the resolved values lives in a later task; this package only
// resolves and returns the effective Config.
package config
