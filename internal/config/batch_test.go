package config

import (
	"strings"
	"testing"
	"time"
)

// TestBatchDefaults pins the batching defaults: a window long enough to group a
// deliberate burst but short enough not to merge separate messages, an absolute
// cap, and a bounded batch size.
func TestBatchDefaults(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("BATCH_WINDOW_MS", "")
	t.Setenv("BATCH_MAX_WAIT_MS", "")
	t.Setenv("BATCH_MAX_MESSAGES", "")
	t.Setenv("CONTINUE_WINDOW_MS", "")
	t.Setenv("ALBUM_WINDOW_MS", "")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if c.BatchWindow != 800*time.Millisecond {
		t.Errorf("BatchWindow = %s; want 800ms", c.BatchWindow)
	}
	if c.BatchMaxWait != 2500*time.Millisecond {
		t.Errorf("BatchMaxWait = %s; want 2.5s", c.BatchMaxWait)
	}
	if c.BatchMaxMessages != 10 {
		t.Errorf("BatchMaxMessages = %d; want 10", c.BatchMaxMessages)
	}
	if c.ContinueWindow != 6*time.Second {
		t.Errorf("ContinueWindow = %s; want 6s", c.ContinueWindow)
	}
	if c.AlbumWindow != 600*time.Millisecond {
		t.Errorf("AlbumWindow = %s; want 600ms", c.AlbumWindow)
	}
}

// TestBatchOverrides: each knob is configurable and parsed as documented.
func TestBatchOverrides(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("BATCH_WINDOW_MS", "1200")
	t.Setenv("BATCH_MAX_WAIT_MS", "4000")
	t.Setenv("BATCH_MAX_MESSAGES", "3")
	t.Setenv("CONTINUE_WINDOW_MS", "1500")
	t.Setenv("ALBUM_WINDOW_MS", "900")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if c.BatchWindow != 1200*time.Millisecond {
		t.Errorf("BatchWindow = %s; want 1.2s", c.BatchWindow)
	}
	if c.BatchMaxWait != 4*time.Second {
		t.Errorf("BatchMaxWait = %s; want 4s", c.BatchMaxWait)
	}
	if c.BatchMaxMessages != 3 {
		t.Errorf("BatchMaxMessages = %d; want 3", c.BatchMaxMessages)
	}
	if c.ContinueWindow != 1500*time.Millisecond {
		t.Errorf("ContinueWindow = %s; want 1.5s", c.ContinueWindow)
	}
	if c.AlbumWindow != 900*time.Millisecond {
		t.Errorf("AlbumWindow = %s; want 900ms", c.AlbumWindow)
	}
}

// TestBatchDisabled: window 0 is the documented escape hatch that restores the
// immediate-delivery behaviour; CONTINUE_WINDOW_MS=0 likewise disables the
// follow-up continuation shortcut.
func TestBatchDisabled(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("BATCH_WINDOW_MS", "0")
	t.Setenv("CONTINUE_WINDOW_MS", "0")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if c.BatchWindow != 0 {
		t.Errorf("BatchWindow = %s; want 0 (disabled)", c.BatchWindow)
	}
	if c.ContinueWindow != 0 {
		t.Errorf("ContinueWindow = %s; want 0 (disabled)", c.ContinueWindow)
	}
}

// TestBatchInvalidValues: a typo fails the boot loudly instead of silently
// disabling batching or toasting a bad batch size.
func TestBatchInvalidValues(t *testing.T) {
	cases := map[string]string{
		"BATCH_WINDOW_MS":    "abc",
		"BATCH_MAX_WAIT_MS":  "-5",
		"BATCH_MAX_MESSAGES": "0",
		"CONTINUE_WINDOW_MS": "nope",
		"ALBUM_WINDOW_MS":    "bad",
	}
	for key, value := range cases {
		key, value := key, value
		t.Run(key+"="+value, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv(key, value)
			_, err := Load()
			if err == nil {
				t.Fatalf("Load() with %s=%q succeeded; want an error", key, value)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error %q does not name %s", err, key)
			}
		})
	}
}

// TestBatchMaxWaitClampedToWindow: a max-wait shorter than the window can never be
// reached before the first extension, so it is clamped instead of failing the boot.
func TestBatchMaxWaitClampedToWindow(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("BATCH_WINDOW_MS", "1000")
	t.Setenv("BATCH_MAX_WAIT_MS", "200")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if c.BatchMaxWait != time.Second {
		t.Errorf("BatchMaxWait = %s; want it clamped to the 1s window", c.BatchMaxWait)
	}
}
