package leader

// Request classes and fair share (ADR-086, feature "admission_classes"),
// ranked at the worker SLOT (ADR-091, admission.go).
//
// A request that finds no free slot for its class waits in the queue of its
// CLASS:
//
//   - critical, standard, sheddable — a freed slot always goes to the highest
//     class that has a request waiting and is below its share (strict order
//     between classes);
//   - inside a class the slot goes round the FLOWS by deficit round robin over
//     requests, so no flow starves another of its class. A flow is the key's
//     team when the key has one, else the key itself;
//   - each class has its own holdMs, maxHeld and maxShare. A class the policy
//     does not list takes the admission-wide holdMs, no cap of its own and its
//     default share (critical 1, standard 1, sheddable 0.25);
//   - admission.maxHeld caps every class together. A request that finds the
//     hold full takes the place of a request of a LOWER class, which is shed;
//     with nothing lower to shed it is shed itself. Overflow and expiry shed
//     the lowest class first, each with exactly the old 503 + Retry-After.
//
// The class is the key's (SnapshotKey.Class in the auth snapshot, held in
// memory by authfile.go); X-Opod-Class may LOWER it, never raise it. With no
// classes in the policy every request is standard (ADR-086 §4) and the same
// gate runs with one class. Nothing here queries the store or counts anything
// per key: a class ranks requests, it does not meter them (ADR-077 §5).
//
// The ranking is per process. Each `--role gateway` door ranks its own queues
// against its own count of what it dispatched; /gatewayz states what that
// means across doors (ADR-086 §6, ADR-091).

import (
	"context"
	"net/http"
	"strings"

	"github.com/opod-io/opod/internal/auth"
)

// The request classes, highest first. The index is the rank.
const (
	classCritical = iota
	classStandard
	classSheddable
	numClasses
)

var classNames = [numClasses]string{"critical", "standard", "sheddable"}

// classHeader is the request header that may lower a request's class.
const classHeader = "X-Opod-Class"

// parseClass maps a class name to its rank. Case and surrounding space are
// ignored; anything else is not a class.
func parseClass(name string) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "critical":
		return classCritical, true
	case "standard":
		return classStandard, true
	case "sheddable":
		return classSheddable, true
	}
	return 0, false
}

// effectiveClass is the class a request is ranked by: the key's class
// (standard when it has none or names no class), lowered by the header when
// the header names a lower class. A header naming a higher class, or no
// class at all, is ignored — a caller may give way, never push ahead.
func effectiveClass(keyClass, header string) int {
	c, ok := parseClass(keyClass)
	if !ok {
		c = classStandard
	}
	if h, ok := parseClass(header); ok && h > c {
		c = h
	}
	return c
}

// classDefaultShare is each class's share of the slots when the policy sets
// none (ADR-091). Sheddable work may fill a quarter of every model's slots:
// the rest stays free for the classes above it, so a critical arrival on an
// endpoint a batch is saturating finds a slot at once instead of waiting for a
// whole batch answer to finish — ordering alone cannot do that. A quarter, not
// half: a free slot is not enough when sheddable sequences decode in the same
// engine batch as the critical one. On a 4-slot llama.cpp worker the
// design-partner cell measured critical p95 TTFT beside a batch at +114 % with
// share 0.5 and −4 % with 0.25 (2026-10-07). The cap is never below one slot,
// so a batch still progresses on the smallest endpoint (four slots: one).
var classDefaultShare = [numClasses]float64{1, 1, 0.25}

// classConfig is the policy's admission half, swapped whole on each policy
// load. Never nil once a policy is applied: with no classes listed it is one
// class, standard, with share 1 — the same gate (ADR-091).
type classConfig struct {
	classed  bool // the policy lists classes; false = every request is standard
	holdMs   [numClasses]int64
	maxHeld  [numClasses]int64   // 0 = no cap of the class's own
	maxShare [numClasses]float64 // in (0, 1]
	total    int64               // every class together; never 0
}

// newClassConfig builds the admission half of a policy. Unknown names are
// skipped (the manager validates them; a leader does not refuse a policy for
// one), a name listed twice keeps its last entry, and a share outside (0, 1]
// is the class's default.
func newClassConfig(p PolicyAdmission) *classConfig {
	cfg := &classConfig{classed: len(p.Classes) > 0, total: int64(max(p.MaxHeld, 0))}
	if cfg.total == 0 {
		cfg.total = defaultMaxHeld
	}
	for i := range cfg.holdMs {
		cfg.holdMs[i] = int64(max(p.HoldMs, 0))
		cfg.maxShare[i] = 1
		if cfg.classed {
			cfg.maxShare[i] = classDefaultShare[i]
		}
	}
	for _, c := range p.Classes {
		if r, ok := parseClass(c.Name); ok {
			cfg.holdMs[r] = int64(max(c.HoldMs, 0))
			cfg.maxHeld[r] = int64(max(c.MaxHeld, 0))
			if c.MaxShare > 0 && c.MaxShare <= 1 {
				cfg.maxShare[r] = c.MaxShare
			} else {
				cfg.maxShare[r] = classDefaultShare[r]
			}
		}
	}
	return cfg
}

// classOf is the class a request is ranked in under this policy: its own
// with classes listed, standard without (ADR-086 §4).
func (c *classConfig) classOf(id admissionIdent) int {
	if !c.classed {
		return classStandard
	}
	return id.class
}

// shareCap is how many of a pool's slots a class may hold at once:
// max(1, floor(share × slots)) — at least one, or a small endpoint (one slot,
// share 0.25) would never serve the class at all.
func (c *classConfig) shareCap(class, slots int) int {
	return max(1, int(c.maxShare[class]*float64(slots)))
}

// admissionIdent is who a request is for the gate: its class and its flow. It
// travels in the request context.
type admissionIdent struct {
	class int
	flow  string
}

type admissionIdentKey struct{}

func withAdmissionIdent(ctx context.Context, id admissionIdent) context.Context {
	return context.WithValue(ctx, admissionIdentKey{}, id)
}

// admissionIdentFrom is the request's ident; a request that carries none is a
// standard request of the anonymous flow.
func admissionIdentFrom(ctx context.Context) admissionIdent {
	if id, ok := ctx.Value(admissionIdentKey{}).(admissionIdent); ok {
		return id
	}
	return admissionIdent{class: classStandard}
}

// keyRank is what the auth snapshot says about one key for admission.
type keyRank struct {
	class string
	team  string
}

// requestIdent reads a request's class and flow from the key the auth
// middleware attached and the in-memory map the auth snapshot built: two map
// reads, no store, no counter.
func (s *Server) requestIdent(r *http.Request) admissionIdent {
	var rank keyRank
	flow := ""
	if k := auth.KeyFrom(r.Context()); k != nil {
		flow = "key:" + k.ID
		if m := s.authf.ranks.Load(); m != nil {
			rank = (*m)[k.ID]
		}
		if rank.team != "" {
			flow = "team:" + rank.team
		}
	}
	return admissionIdent{class: effectiveClass(rank.class, r.Header.Get(classHeader)), flow: flow}
}

// ---- the queues: pure, no locks, no clock ------------------------------------

// heldReq is one request waiting in a class queue.
type heldReq struct {
	class int
	flow  string
	// done is closed when the request leaves the queue by the gate's hand:
	// granted a slot, or shed (shedWhy says why). Written under slotGate.mu.
	done    chan struct{}
	queued  bool
	granted bool
	shedWhy string // "evicted" | "unavailable"
	reason  string // the 503 text when shed for "unavailable"
}

// drrQuantum is the credit a flow earns on each visit, and one request costs
// one. Deficit round robin over requests of unit cost visits each flow with
// work once per round, so no flow waits more than one round of its class.
const drrQuantum = 1

// flowQueue is one flow's waiting requests, oldest first.
type flowQueue struct {
	reqs    []*heldReq
	deficit int
}

// drrQueue is one class: its flows, and the round they are visited in.
type drrQueue struct {
	flows map[string]*flowQueue
	ring  []string // flows with work, in round order
	pos   int      // the flow whose visit is next
	n     int
}

func (q *drrQueue) flow(id string) *flowQueue {
	if q.flows == nil {
		q.flows = map[string]*flowQueue{}
	}
	f, ok := q.flows[id]
	if !ok {
		// A flow that starts waiting joins at the END of the current round —
		// just behind the flow whose visit is next — so a flow that left the
		// round and came back cannot be visited again ahead of one that never
		// left it.
		f = &flowQueue{}
		q.flows[id] = f
		if q.pos > len(q.ring) {
			q.pos = len(q.ring)
		}
		q.ring = append(q.ring, "")
		copy(q.ring[q.pos+1:], q.ring[q.pos:])
		q.ring[q.pos] = id
		q.pos++
	}
	return f
}

func (q *drrQueue) push(h *heldReq) {
	f := q.flow(h.flow)
	f.reqs = append(f.reqs, h)
	q.n++
}

// pop takes the next request by deficit round robin, or nil.
func (q *drrQueue) pop() *heldReq {
	if q.n == 0 {
		return nil
	}
	if q.pos >= len(q.ring) {
		q.pos = 0
	}
	f := q.flows[q.ring[q.pos]]
	if f.deficit < 1 {
		f.deficit += drrQuantum // the visit's credit
	}
	h := f.reqs[0]
	f.reqs = f.reqs[1:]
	f.deficit--
	q.n--
	if len(f.reqs) == 0 {
		q.dropFlow(q.pos) // the next flow slides into pos
	} else if f.deficit < 1 {
		q.pos++
	}
	return h
}

// remove takes h out wherever it waits; false when it is not queued here.
func (q *drrQueue) remove(h *heldReq) bool {
	f, ok := q.flows[h.flow]
	if !ok {
		return false
	}
	for i, r := range f.reqs {
		if r != h {
			continue
		}
		f.reqs = append(f.reqs[:i], f.reqs[i+1:]...)
		q.n--
		if len(f.reqs) == 0 {
			for j, id := range q.ring {
				if id == h.flow {
					q.dropFlow(j)
					break
				}
			}
		}
		return true
	}
	return false
}

// newestOfLargest is the request to shed from this class to make room: the
// most recent of the flow holding the most, so a shed lands on whoever is
// taking the biggest share. nil when the class is empty.
func (q *drrQueue) newestOfLargest() *heldReq {
	var best *flowQueue
	for _, id := range q.ring {
		if f := q.flows[id]; best == nil || len(f.reqs) > len(best.reqs) {
			best = f
		}
	}
	if best == nil || len(best.reqs) == 0 {
		return nil
	}
	return best.reqs[len(best.reqs)-1]
}

func (q *drrQueue) dropFlow(i int) {
	delete(q.flows, q.ring[i])
	q.ring = append(q.ring[:i], q.ring[i+1:]...)
	if q.pos > i {
		q.pos--
	}
	if q.pos >= len(q.ring) {
		q.pos = 0
	}
}

// classQueues is the classed hold's waiting room: one DRR queue per class.
type classQueues struct {
	q [numClasses]drrQueue
}

func (c *classQueues) push(h *heldReq)        { c.q[h.class].push(h) }
func (c *classQueues) remove(h *heldReq) bool { return c.q[h.class].remove(h) }

func (c *classQueues) total() int {
	n := 0
	for i := range c.q {
		n += c.q[i].n
	}
	return n
}

// pop is the next request to get a slot: the highest class with one waiting,
// and inside it the next flow's oldest.
func (c *classQueues) pop() *heldReq {
	return c.popEligible(func(int) bool { return true })
}

// popEligible is pop over the classes eligible says may take a slot now: a
// class at its share is passed over and the next class is tried, so a capped
// class never idles a slot a lower class could use.
func (c *classQueues) popEligible(eligible func(class int) bool) *heldReq {
	for i := range c.q {
		if c.q[i].n == 0 || !eligible(i) {
			continue
		}
		if h := c.q[i].pop(); h != nil {
			return h
		}
	}
	return nil
}

// victimBelow is the request to shed so one of class can wait: from the
// lowest class below it that has any. nil when nothing below waits.
func (c *classQueues) victimBelow(class int) *heldReq {
	for i := numClasses - 1; i > class; i-- {
		if h := c.q[i].newestOfLargest(); h != nil {
			return h
		}
	}
	return nil
}
