package main

import "testing"

func TestResolveNofileLimit(t *testing.T) {
	t.Setenv("HOTBED_NOFILE", "")
	if got := resolveNofileLimit(0); got != defaultGuestNofile {
		t.Fatalf("default resolveNofileLimit(0) = %d, want %d", got, defaultGuestNofile)
	}

	t.Setenv("HOTBED_NOFILE", "131072")
	if got := resolveNofileLimit(0); got != 131072 {
		t.Fatalf("env resolveNofileLimit(0) = %d, want 131072", got)
	}

	// Explicit flag overrides env.
	if got := resolveNofileLimit(262144); got != 262144 {
		t.Fatalf("flag resolveNofileLimit(262144) = %d, want 262144", got)
	}

	// Invalid env falls back cleanly to default.
	t.Setenv("HOTBED_NOFILE", "not-a-number")
	if got := resolveNofileLimit(0); got != defaultGuestNofile {
		t.Fatalf("invalid env resolveNofileLimit(0) = %d, want %d", got, defaultGuestNofile)
	}
}
