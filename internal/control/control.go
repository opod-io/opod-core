// Package control holds the mutating operations of the Opod control
// plane in one place. Both the CLI commands in cmd/opod and the admin
// HTTP endpoints in internal/controlplane/admin_*.go invoke functions from this
// package, so behaviour is identical across surfaces (same audit log
// entry, same validation, same errors).
//
// New mutating operations should be added here first, then surfaced as
// a `opod <command>` CLI verb in cmd/opod/ and (if appropriate) as an
// admin HTTP endpoint in internal/controlplane/admin_*.go. The CLI command is a
// thin arg parser; the HTTP endpoint is a thin request decoder. Neither
// reimplements logic. See ARCHITECTURE.md ("CLI / Admin API / Web UI
// contract") and TASKS.md M4-T20 for the rationale.
package control
