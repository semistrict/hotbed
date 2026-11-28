//go:build darwin

package worker

import (
	"encoding/binary"
	"errors"
	"math"
	"testing"
)

func TestDarwinSystemCPU(t *testing.T) {
	var sys darwinSystem
	cpu, err := sys.CPU()
	if err != nil {
		t.Fatalf("darwinSystem.CPU() err = %v, want nil", err)
	}
	if cpu.Busy < 0 || cpu.Busy > 1 || math.IsNaN(cpu.Busy) {
		t.Fatalf("darwinSystem.CPU().Busy = %v, want in [0, 1]", cpu.Busy)
	}
	if _, err := sys.Pressure("cpu"); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("darwinSystem.Pressure(cpu) err = %v, want ErrUnsupported", err)
	}
}

func TestParseDarwinLoadavg(t *testing.T) {
	makeRaw := func(ld0 uint32, fscale int64) []byte {
		raw := make([]byte, 24)
		binary.LittleEndian.PutUint32(raw[0:4], ld0)
		binary.LittleEndian.PutUint64(raw[16:24], uint64(fscale))
		return raw
	}

	got, err := parseDarwinLoadavg(makeRaw(2048*4, 2048), 8)
	if err != nil || math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("parseDarwinLoadavg(4.0 / 8) = (%v, %v), want (0.5, nil)", got, err)
	}
	got, err = parseDarwinLoadavg(makeRaw(2048*24, 2048), 8)
	if err != nil || got != 1 {
		t.Fatalf("parseDarwinLoadavg(24.0 / 8) = (%v, %v), want (1, nil)", got, err)
	}
	got, err = parseDarwinLoadavg(makeRaw(0, 2048), 8)
	if err != nil || got != 0 {
		t.Fatalf("parseDarwinLoadavg(0 / 8) = (%v, %v), want (0, nil)", got, err)
	}
	if _, err := parseDarwinLoadavg(make([]byte, 16), 8); err == nil {
		t.Fatal("parseDarwinLoadavg(short) = nil, want error")
	}
	if _, err := parseDarwinLoadavg(makeRaw(1024, 0), 8); err == nil {
		t.Fatal("parseDarwinLoadavg(fscale=0) = nil, want error")
	}
}
