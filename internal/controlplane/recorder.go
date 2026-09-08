package controlplane

// record (E2, one emit): a lifecycle event is written ONCE — to the durable
// event log the control plane pulls (/admin/v1/events/stream) AND to the
// in-process bus the dashboard-era refresh hints ride on — from one call.
// The bus topic follows the event type's family (node.* → nodes, model.* →
// models, shard.* → shards, everything else → audit), so a caller never
// pairs a logEvent with a hand-picked Publish again.

import (
	"strings"

	"github.com/opod-io/opod/internal/events"
)

func topicFor(typ string) events.Topic {
	switch strings.SplitN(typ, ".", 2)[0] {
	case "node", "worker":
		return events.TopicNodes
	case "model", "plan":
		return events.TopicModels
	case "shard":
		return events.TopicShards
	case "usage":
		return events.TopicUsage
	}
	return events.TopicAudit
}

// record appends to the event log and publishes the matching bus event.
func (s *Server) record(typ, subject string, data map[string]any) {
	s.logEvent(typ, subject, data)
	if s.bus != nil {
		s.bus.Publish(events.Event{Topic: topicFor(typ), ID: subject})
	}
}
