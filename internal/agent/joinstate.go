package agent

// Whether this worker is joined to its leader (feature "worker_readyz",
// PLAN T24.34).
//
// A worker whose engine is up and whose leader refuses it serves nothing: the
// leader routes only to workers it accepted. Kubernetes read such a pod Ready
// anyway — its probe asked the engine — so a disruption budget counted it as
// capacity and let a drain take the one worker that was serving. The join
// state is the half of readiness only this process knows: the leader's answer
// to its last register or heartbeat.

import (
	"fmt"
	"sync"
)

// JoinState is the leader's answer to the last register or heartbeat. One
// value is shared by the Agent, which writes it, and the Server, which serves
// it on /readyz. The zero value is "not joined yet". Methods are nil-safe.
type JoinState struct {
	mu     sync.Mutex
	joined bool
	why    string
}

// note records the answer to one call to the leader.
func (j *JoinState) note(call string, code int, err error) {
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.joined = err == nil
	switch {
	case err == nil:
		j.why = ""
	case code != 0:
		j.why = fmt.Sprintf("the leader answered the %s with HTTP %d: %v", call, code, err)
	default:
		j.why = fmt.Sprintf("the %s did not reach the leader: %v", call, err)
	}
}

// Joined reports whether the leader accepted the last register or heartbeat,
// and why not when it did not.
func (j *JoinState) Joined() (bool, string) {
	if j == nil {
		return false, "this worker keeps no join state"
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.joined && j.why == "" {
		return false, "not registered with the leader yet"
	}
	return j.joined, j.why
}
