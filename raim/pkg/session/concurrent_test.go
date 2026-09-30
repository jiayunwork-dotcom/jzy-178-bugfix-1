package session_test

import (
	"fmt"
	"sync"
	"testing"

	"raim/internal/sim"
)

// 多个 goroutine 各向独立会话按序提交，压共享互斥锁与文件系统：
// 不应有数据竞争、死锁或丢失，每个会话历元数完整。
func TestConcurrentAppend(t *testing.T) {
	st := newStore(t)
	const workers, perSession = 12, 30
	rec := sim.DefaultReceiver()
	c := sim.EvenSky(rec, 9, 15, 5)

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		w := w
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("c%d", w)
			if _, err := st.CreateSession(id, "enroute"); err != nil {
				errs <- err
				return
			}
			epochs := genEpochs(c, perSession, 1, int64(w+1), nil)
			for _, e := range epochs {
				if _, err := st.AppendEpoch(id, e); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发提交出错: %v", err)
	}
	for w := 0; w < workers; w++ {
		sess, err := st.GetSession(fmt.Sprintf("c%d", w))
		if err != nil {
			t.Fatal(err)
		}
		if sess.Stats.Epochs != perSession {
			t.Fatalf("会话 c%d 历元数=%d，期望 %d", w, sess.Stats.Epochs, perSession)
		}
	}
}
