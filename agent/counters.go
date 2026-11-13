package agent

import "sync/atomic"

// counters are one connection's flow and byte counts. Flows update them
// concurrently.
type counters struct {
	flowsActive atomic.Int64
	flowsTotal  atomic.Uint64
	bytesOut    atomic.Uint64
	bytesIn     atomic.Uint64
}

// counterValues is a snapshot of counters.
type counterValues struct {
	flowsActive uint64
	flowsTotal  uint64
	bytesOut    uint64
	bytesIn     uint64
}

func (c *counters) load() counterValues {
	return counterValues{
		flowsActive: uint64(max(c.flowsActive.Load(), 0)),
		flowsTotal:  c.flowsTotal.Load(),
		bytesOut:    c.bytesOut.Load(),
		bytesIn:     c.bytesIn.Load(),
	}
}

func (v counterValues) add(w counterValues) counterValues {
	return counterValues{
		flowsActive: v.flowsActive + w.flowsActive,
		flowsTotal:  v.flowsTotal + w.flowsTotal,
		bytesOut:    v.bytesOut + w.bytesOut,
		bytesIn:     v.bytesIn + w.bytesIn,
	}
}

// flowOpened counts a flow the agent answered with OK. The returned func
// counts its end.
func (c *counters) flowOpened() (closed func()) {
	c.flowsTotal.Add(1)
	c.flowsActive.Add(1)
	return func() { c.flowsActive.Add(-1) }
}
