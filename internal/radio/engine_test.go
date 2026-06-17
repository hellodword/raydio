package radio

import (
	"context"
	"testing"
)

func TestAudioRingSkipsSlowSubscriberToOldestBufferedPacket(t *testing.T) {
	r := newAudioRing(2)
	r.publish([]byte("a"))
	r.publish([]byte("b"))
	r.publish([]byte("c"))

	p, next, err := r.wait(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.Seq != 1 || string(p.Data) != "b" {
		t.Fatalf("packet = seq %d data %q, want seq 1 data b", p.Seq, p.Data)
	}
	if next != 2 {
		t.Fatalf("next = %d, want 2", next)
	}
}

func TestAudioRingLiveSeqStartsAtLatestPacket(t *testing.T) {
	r := newAudioRing(4)
	if got := r.liveSeq(); got != 0 {
		t.Fatalf("empty live seq = %d, want 0", got)
	}
	r.publish([]byte("a"))
	r.publish([]byte("b"))

	p, next, err := r.wait(context.Background(), r.liveSeq())
	if err != nil {
		t.Fatal(err)
	}
	if p.Seq != 1 || string(p.Data) != "b" {
		t.Fatalf("packet = seq %d data %q, want seq 1 data b", p.Seq, p.Data)
	}
	if next != 2 {
		t.Fatalf("next = %d, want 2", next)
	}
}

func TestFramesForDurationRoundsUpToWholeFrame(t *testing.T) {
	if got := framesForDuration(frameDuration(10)); got != 10 {
		t.Fatalf("exact frames = %d, want 10", got)
	}
	if got := framesForDuration(frameDuration(10) + 1); got != 11 {
		t.Fatalf("rounded frames = %d, want 11", got)
	}
}
