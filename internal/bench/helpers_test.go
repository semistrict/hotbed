package bench

import (
	"slices"
	"strings"
)

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func diff(a, b map[string]string) string {
	var out []string
	for k, v := range a {
		if w, ok := b[k]; !ok {
			out = append(out, "- "+k)
		} else if w != v {
			out = append(out, "~ "+k+": "+v+" -> "+w)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			out = append(out, "+ "+k)
		}
	}
	slices.Sort(out)
	return strings.Join(out, "\n")
}
