package worker

import (
	"os"
	"regexp"
	"slices"
	"testing"
)

// The cockpit knows every kind of feed line the worker writes: one it does
// not know would show with no glyph, and one that reuses another's name
// mixes the two (a node's own hold ended, or a paused command resumed).
func TestTheCockpitKnowsEveryFeedKind(t *testing.T) {
	src, err := os.ReadFile("../../ui/src/components/EventFeed.tsx")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)const KIND\b[^{]*\{(.*?)\n\};`).FindSubmatch(src)
	if m == nil {
		t.Fatal("ui/src/components/EventFeed.tsx has no KIND glyph map")
	}
	var ui []string
	for _, k := range regexp.MustCompile(`(?m)^\s*([a-z]+)\s*:`).FindAllSubmatch(m[1], -1) {
		ui = append(ui, string(k[1]))
	}
	seen := map[feedKind]bool{}
	for _, k := range feedKinds {
		if seen[k] {
			t.Errorf("feed kind %q is listed twice", k)
		}
		seen[k] = true
		if !slices.Contains(ui, string(k)) {
			t.Errorf("the cockpit's EventFeed KIND map (%q) lacks the worker's feed kind %q", ui, k)
		}
	}
}
