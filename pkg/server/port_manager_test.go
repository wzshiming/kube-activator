package server

import (
	"errors"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// regression test for #11: a failed scale-up must be retried so the
// already-accepted connections are not stranded forever
func TestScaleUpRetriedAfterFailure(t *testing.T) {
	var calls atomic.Int32
	pm := NewPortManager(func(pi *PortInformation) error {
		if calls.Add(1) < 3 {
			return errors.New("transient failure")
		}
		return nil
	})

	pi, err := pm.AddTarget("svc", "ns", 80)
	if err != nil {
		t.Fatal(err)
	}
	defer pm.RemoveTarget("svc", "ns", 80)

	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(pi.Listener.Port())))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if calls.Load() >= 3 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("callback was not retried until success, calls=%d", calls.Load())
}

// retries must stop once the target is removed
func TestScaleUpRetryStopsAfterRemoval(t *testing.T) {
	var calls atomic.Int32
	pm := NewPortManager(func(pi *PortInformation) error {
		calls.Add(1)
		return errors.New("permanent failure")
	})

	pi, err := pm.AddTarget("svc", "ns", 80)
	if err != nil {
		t.Fatal(err)
	}

	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(pi.Listener.Port())))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// wait for the first attempt, then remove the target
	deadline := time.Now().Add(10 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() == 0 {
		t.Fatal("callback was never invoked")
	}
	pm.RemoveTarget("svc", "ns", 80)

	// after removal the retry loop finishes its in-flight backoff at most
	// once more, then must stop
	time.Sleep(500 * time.Millisecond)
	settled := calls.Load()
	time.Sleep(1 * time.Second)
	if got := calls.Load(); got > settled+1 {
		t.Fatalf("callback still retried after target removal, settled=%d got=%d", settled, got)
	}
}
