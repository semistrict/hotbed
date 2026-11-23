package profiler

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"

	"google.golang.org/protobuf/proto"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// maxStoredProfileBytes bounds a stored profile once decompressed.
const maxStoredProfileBytes = 16 << 20

// EncodeProfile is a profile as it is stored, on a worker's disk and in the
// controller's database: gzip-compressed protobuf.
func EncodeProfile(p *pb.CommandProfile) ([]byte, error) {
	raw, err := proto.Marshal(p)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DecodeProfile reads a profile EncodeProfile stored, by this build or an
// older one, admitted (Admit).
func DecodeProfile(stored []byte) (*pb.CommandProfile, error) {
	gz, err := gzip.NewReader(bytes.NewReader(stored))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	raw, err := io.ReadAll(io.LimitReader(gz, maxStoredProfileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxStoredProfileBytes {
		return nil, errors.New("stored profile is over 16 MiB")
	}
	p := &pb.CommandProfile{}
	if err := proto.Unmarshal(raw, p); err != nil {
		return nil, err
	}
	if err := Admit(p); err != nil {
		return nil, err
	}
	return p, nil
}

// SaveProfile writes a profile to path atomically.
func SaveProfile(path string, p *pb.CommandProfile) error {
	if p == nil {
		return nil
	}
	b, err := EncodeProfile(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// LoadProfile reads a profile SaveProfile wrote.
func LoadProfile(path string) (*pb.CommandProfile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return DecodeProfile(b)
}

// IsNotExist reports whether err is a missing profile file.
func IsNotExist(err error) bool { return errors.Is(err, os.ErrNotExist) }
