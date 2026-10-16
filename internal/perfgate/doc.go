// Package perfgate holds the leader-cost gate: a pull request fails when the
// leader starts costing more time to first token than it did.
//
// The test builds the real binary and runs `opod up` and `opod join` against a
// stub engine with a fixed token cadence, then measures the same stub two ways
// on the same machine, interleaved: called directly (A) and through the leader
// and the worker (B). Only the paired difference B − A is judged — an absolute
// number would measure the runner, not the leader. A third arm puts a leader
// that is a fixed 20 ms slower in front (S) and requires the gate to fail it,
// so the gate proves on every run that it can still see a regression.
//
// It runs only with the build tag, never inside `go test ./...`:
//
//	go test -tags perfgate -count=1 -v ./internal/perfgate/
package perfgate
