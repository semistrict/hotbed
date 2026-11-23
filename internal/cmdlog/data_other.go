//go:build !linux && !darwin

package cmdlog

import "os"

func DataBytes(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}
