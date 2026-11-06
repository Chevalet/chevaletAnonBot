package bot

import (
	"testing"
	"time"
)

// Benchmarks for the delivery queue's hot path: adding a burst and flushing it.
// They guard against accidentally quadratic per-item work as batching grows.
func BenchmarkQueueBurst10(b *testing.B) {
	for i := 0; i < b.N; i++ {
		q := newBatcher(5*time.Millisecond, 5*time.Millisecond, time.Second, 1000, func(convKey, []queueUnit) {})
		key := convKey{senderUID: "s", targetUID: "t"}
		for j := 0; j < 10; j++ {
			q.AddSingle(key, batchItem{msg: dummyMessage(int64(j))}, batchReply{})
		}
		q.FlushKey(key)
		q.Shutdown(time.Second)
	}
}

func BenchmarkQueueAlbumsAndMessages(b *testing.B) {
	for i := 0; i < b.N; i++ {
		q := newBatcher(5*time.Millisecond, 5*time.Millisecond, time.Second, 1000, func(convKey, []queueUnit) {})
		key := convKey{senderUID: "s", targetUID: "t"}
		for j := 0; j < 10; j++ {
			q.AddAlbumItem(key, "g", dummyMessage(int64(100+j)), "", batchReply{})
			q.AddSingle(key, batchItem{msg: dummyMessage(int64(200 + j))}, batchReply{})
		}
		q.FlushKey(key)
		q.Shutdown(time.Second)
	}
}
