package events

import (
	apiEvents "gocache/api/events"
	gcpcv1 "gocache/api/gcpc/v1"
	"google.golang.org/protobuf/proto"
)

func cloneEvent(evt apiEvents.Event) apiEvents.Event {
	if evt.Proto == nil {
		return evt
	}
	cloned, ok := proto.Clone(evt.Proto).(*gcpcv1.EventV1)
	if !ok {
		return evt
	}
	return apiEvents.Event{Proto: cloned}
}
