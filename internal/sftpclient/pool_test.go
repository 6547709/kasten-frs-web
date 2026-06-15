package sftpclient

import (
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

func newTestPool(t *testing.T) *Pool {
	t.Helper()
	ts, cleanup := StartSFTPTestServer(t)
	t.Cleanup(cleanup)
	c, _ := NewClient(ClientConfig{
		Username:   "root",
		Signer:     ts.Signer(),
		HostKeySig: "[" + ts.Addr().String() + "] " + ts.HostKeyString(),
	})
	return NewPool(c, 50*time.Millisecond)
}

func TestPool_StoreGetClose(t *testing.T) {
	p := newTestPool(t)
	key := SessionKey{
		UserSessionID: "u1",
		FRS:           types.NamespacedName{Namespace: "ns", Name: "frs"},
	}
	sess := &Session{}
	p.Store(key, sess)
	got, ok := p.Get(key)
	if !ok || got != sess {
		t.Fatal("expected stored session")
	}
	p.Close(key)
	_, ok = p.Get(key)
	if ok {
		t.Fatal("expected closed")
	}
}

func TestPool_TTLExpiry(t *testing.T) {
	p := newTestPool(t)
	key := SessionKey{UserSessionID: "u", FRS: types.NamespacedName{Namespace: "ns", Name: "f"}}
	p.Store(key, &Session{})
	time.Sleep(80 * time.Millisecond)
	if _, ok := p.Get(key); ok {
		t.Fatal("expected TTL expiry")
	}
}

func TestPool_Concurrent(t *testing.T) {
	p := newTestPool(t)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := SessionKey{UserSessionID: "u", FRS: types.NamespacedName{Namespace: "ns", Name: "f"}}
			p.Store(key, &Session{})
			_, _ = p.Get(key)
		}(i)
	}
	wg.Wait()
}
