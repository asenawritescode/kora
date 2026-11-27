//go:build !qjsscript

package script

// qjsParity is false when goja is the embedded runtime (the default build).
const qjsParity = false
