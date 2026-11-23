package baseimage

import "time"

// Test exports used by external package baseimage_test to avoid an import cycle
// with internal/fake (which imports baseimage for fake.BaseImage).

const MaxReadyMarkerBytes = maxReadyMarkerBytes

var (
	WriteReadyMarker = writeReadyMarker
	ParseSubmounts   = parseSubmounts
)

func (m *Manager) SetTestHooks(unmount func(string) error, retryDelay time.Duration) {
	m.unmount = unmount
	m.retryDelay = retryDelay
}
