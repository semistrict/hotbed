package bench

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// CPUs is a set of CPU numbers, sorted, each once.
type CPUs []int

// ParseCPUs reads a cpulist, as the kernel writes them (4-7,12-15).
func ParseCPUs(s string) (CPUs, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out CPUs
	for _, part := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, err1 := strconv.ParseUint(lo, 10, 16)
		b := a
		var err2 error
		if isRange {
			b, err2 = strconv.ParseUint(hi, 10, 16)
		}
		if err1 != nil || err2 != nil || b < a {
			return nil, fmt.Errorf("%q is not a cpulist, such as 4-7,12-15", s)
		}
		for c := a; c <= b; c++ {
			out = append(out, int(c))
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// String is the set as a cpulist, runs as ranges.
func (cs CPUs) String() string {
	var b strings.Builder
	for i := 0; i < len(cs); {
		j := i
		for j+1 < len(cs) && cs[j+1] == cs[j]+1 {
			j++
		}
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(cs[i]))
		if j > i {
			b.WriteByte('-')
			b.WriteString(strconv.Itoa(cs[j]))
		}
		i = j + 1
	}
	return b.String()
}

// Mask is the set as a hex cpumask, in 32-bit words, most significant first,
// comma-separated, as /proc/irq/default_smp_affinity and the workqueues'
// cpumask take it.
func (cs CPUs) Mask() string {
	if len(cs) == 0 {
		return "00000000"
	}
	words := make([]uint32, cs[len(cs)-1]/32+1)
	for _, c := range cs {
		words[c/32] |= 1 << (c % 32)
	}
	parts := make([]string, len(words))
	for i, w := range words {
		parts[len(words)-1-i] = fmt.Sprintf("%08x", w)
	}
	return strings.Join(parts, ",")
}

// Minus is the CPUs of cs not in o.
func (cs CPUs) Minus(o CPUs) CPUs {
	var out CPUs
	for _, c := range cs {
		if !slices.Contains(o, c) {
			out = append(out, c)
		}
	}
	return out
}

// Intersects says whether cs and o share a CPU.
func (cs CPUs) Intersects(o CPUs) bool {
	return slices.ContainsFunc(cs, func(c int) bool { return slices.Contains(o, c) })
}

// ParseMask reads a hex cpumask, in comma-separated 32-bit words, most
// significant first, padded or not (7ff, 00000002,00000001).
func ParseMask(s string) (CPUs, error) {
	words := strings.Split(strings.TrimSpace(s), ",")
	var out CPUs
	for i, w := range words {
		v, err := strconv.ParseUint(w, 16, 32)
		if err != nil {
			return nil, fmt.Errorf("%q is not a cpumask", s)
		}
		base := 32 * (len(words) - 1 - i)
		for b := range 32 {
			if v&(1<<b) != 0 {
				out = append(out, base+b)
			}
		}
	}
	slices.Sort(out)
	return out, nil
}
