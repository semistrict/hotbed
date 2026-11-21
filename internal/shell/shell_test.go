package shell

import "testing"

func TestJoin(t *testing.T) {
	got := Join([]string{"cargo", "build", "--features=a,b", "it's", "", "a b"})
	want := `cargo build --features=a,b 'it'\''s' '' 'a b'`
	if got != want {
		t.Errorf("Join = %s, want %s", got, want)
	}
}
