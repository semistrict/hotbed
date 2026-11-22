// A live log follows its newest line only while the reader is at its end:
// scrolling up to read something stops the following, and scrolling back
// down resumes it.

type Scroll = { scrollTop: number; scrollHeight: number; clientHeight: number };

// The slack absorbs subpixel scroll positions on zoomed or high-DPI screens.
const SLACK = 4;

export function atEnd({ scrollTop, scrollHeight, clientHeight }: Scroll): boolean {
  return scrollHeight - scrollTop - clientHeight <= SLACK;
}
