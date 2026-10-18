package api

import (
	"testing"
	"time"
)

func TestLoginThrottleBlocksAfterLimit(t *testing.T) {
	now := time.Now()
	throttle := newLoginThrottle()
	throttle.now = func() time.Time { return now }

	for i := 0; i < loginAttemptLimit; i++ {
		if ok, _ := throttle.allow("10.0.0.5"); !ok {
			t.Fatalf("attempt %d was refused before the limit", i+1)
		}
	}

	ok, retry := throttle.allow("10.0.0.5")
	if ok {
		t.Fatal("an attempt over the limit must be refused")
	}
	if retry <= 0 || retry > loginAttemptWindow {
		t.Fatalf("retry after = %v, want a value inside the window", retry)
	}

	// A different client keeps its own bucket.
	if ok, _ := throttle.allow("10.0.0.6"); !ok {
		t.Fatal("one client's attempts must not throttle another")
	}
}

func TestLoginThrottleResetsAfterWindow(t *testing.T) {
	now := time.Now()
	throttle := newLoginThrottle()
	throttle.now = func() time.Time { return now }

	for i := 0; i < loginAttemptLimit; i++ {
		throttle.allow("10.0.0.5")
	}
	if ok, _ := throttle.allow("10.0.0.5"); ok {
		t.Fatal("expected the bucket to be exhausted")
	}

	now = now.Add(loginAttemptWindow + time.Second)
	if ok, _ := throttle.allow("10.0.0.5"); !ok {
		t.Fatal("a new window must start a fresh bucket")
	}
}

func TestLoginThrottleResetClearsCounter(t *testing.T) {
	throttle := newLoginThrottle()
	for i := 0; i < loginAttemptLimit; i++ {
		throttle.allow("10.0.0.5")
	}
	throttle.reset("10.0.0.5")
	if ok, _ := throttle.allow("10.0.0.5"); !ok {
		t.Fatal("a successful login must clear the client's counter")
	}
}

func TestLoginThrottleNilIsPermissive(t *testing.T) {
	var throttle *loginThrottle
	if ok, _ := throttle.allow("x"); !ok {
		t.Fatal("a nil throttle must not block requests")
	}
	throttle.reset("x") // must not panic
}
