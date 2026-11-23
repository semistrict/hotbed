//go:build !linux

package worker

// logWatcher is a no-op outside Linux; FollowLog still keeps its log file
// descriptor open and falls back to its timer.
type logWatcher struct{}

func newLogWatcher(string) *logWatcher      { return nil }
func (*logWatcher) Events() <-chan struct{} { return nil }
func (*logWatcher) Close()                  {}
