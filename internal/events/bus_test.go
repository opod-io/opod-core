package events

import (
	"testing"
	"time"
)

func TestBus_PublishFansToAllSubscribers(t *testing.T) {
	b := New()
	a, cancelA := b.Subscribe(8)
	c, cancelC := b.Subscribe(8)
	defer cancelA()
	defer cancelC()

	b.Publish(Event{Topic: TopicModels, ID: "qwen3.6-27b"})

	for i, ch := range []<-chan Event{a, c} {
		select {
		case ev := <-ch:
			if ev.Topic != TopicModels || ev.ID != "qwen3.6-27b" {
				t.Errorf("subscriber %d got %+v, want models/qwen3.6-27b", i, ev)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d: timed out waiting for event", i)
		}
	}
}

func TestBus_CancelRemovesSubscriber(t *testing.T) {
	b := New()
	_, cancel := b.Subscribe(8)
	if b.Len() != 1 {
		t.Fatalf("before cancel: len=%d", b.Len())
	}
	cancel()
	if b.Len() != 0 {
		t.Fatalf("after cancel: len=%d", b.Len())
	}
}

func TestBus_SlowSubscriberDoesNotBlockPublisher(t *testing.T) {
	b := New()
	// Buffer of 2; we'll publish 100 events. The slow subscriber should
	// see at most ~2; the publisher must never block.
	_, cancel := b.Subscribe(2)
	defer cancel()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			b.Publish(Event{Topic: TopicUsage})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked despite slow subscriber")
	}
}
