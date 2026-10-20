package agent

import "testing"

// PLAN T23.2: llama-server does not report its KV layout, and its pool cannot
// be read without it, so the worker tells the driver what its own launch line
// says. The last word wins, as on llama-server's command line.
func TestTheLaunchLinesKVLayout(t *testing.T) {
	for _, c := range []struct {
		flags EngineFlags
		want  bool
		why   string
	}{
		{EngineFlags{}, true, "slot count automatic: the build shares one pool"},
		{EngineFlags{"parallel": "4"}, false, "a stated slot count: a partition per slot"},
		{EngineFlags{"extra": "-np 4"}, false, "a slot count in extra counts too"},
		{EngineFlags{"parallel": "4", "kv_unified": "true"}, true, "the manager's flag"},
		{EngineFlags{"kv_unified": "false"}, false, "the manager's flag"},
		{EngineFlags{"kv_unified": "false", "extra": "--kv-unified"}, true, "extra is written last and wins"},
		{EngineFlags{"kv_unified": "true", "extra": "-kvu --no-kv-unified"}, false, "the last word in extra"},
	} {
		if got, known := c.flags.llamaKVUnified(); got != c.want || !known {
			t.Errorf("%v: unified=%v known=%v, want %v (%s)", c.flags, got, known, c.want, c.why)
		}
	}
	// --kv-unified-per-slot caps each slot: /props then reports the cap, not
	// the pool, so the layout is not told and the driver states no pool.
	if _, known := (EngineFlags{"kv_unified": "true", "extra": "--kv-unified-per-slot 8192"}).llamaKVUnified(); known {
		t.Error("a per-slot cap must leave the layout untold")
	}
}
