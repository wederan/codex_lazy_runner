package store

import (
	"sync"
	"testing"
)

func TestPutFindAndWake(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, id := range []string{"abc123", "abd456", "fff000"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Put(&Task{ID: id, SessionID: "sess-" + id, Status: StatusWaiting}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Tasks) != 3 {
		t.Fatalf("tasks = %d", len(st.Tasks))
	}
	if task, _ := st.Find("abd"); task == nil || task.ID != "abd456" {
		t.Errorf("prefix lookup = %+v", task)
	}
	if task, _ := st.Find("sess-fff000"); task == nil || task.ID != "fff000" {
		t.Errorf("session lookup = %+v", task)
	}
	if _, err := st.Find("ab"); err == nil {
		t.Error("ambiguous prefix accepted")
	}
	if task, err := st.Find("zzz"); task != nil || err != nil {
		t.Error("unknown ref matched")
	}

	s.RequestWake("abc123", WakeForce)
	if mode, ok := s.PeekWake("abc123"); !ok || mode != WakeForce {
		t.Fatalf("peek = %q %v", mode, ok)
	}
	if mode, ok := s.TakeWake("abc123"); !ok || mode != WakeForce {
		t.Fatalf("take = %q %v", mode, ok)
	}
	if _, ok := s.TakeWake("abc123"); ok {
		t.Fatal("wake request not consumed")
	}
}
