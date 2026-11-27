package worker

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/cmdlog"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
)

const (
	outputPushPollInterval = 250 * time.Millisecond
	outputPushBatchBytes   = 256 << 10
	outputPushMaxChunks    = 256
	outputPushRPCTimeout   = 15 * time.Second
)

type outputStream struct {
	offset  int64
	eofSent bool
	tsIdx   *cmdlog.TimestampIndex
}

// outputPusher incrementally pushes command output chunks (with per-chunk
// receive timestamps and logical byte offsets) to each command's CommandDO
// via ClusterService.PushCommandOutput as output arrives and on completion.
type outputPusher struct {
	eng         *Engine
	cluster     hotbedv1connect.ClusterServiceClient
	streams     map[string]*outputStream
	buf         []byte
	unsupported bool
}

func newOutputPusher(eng *Engine, cluster hotbedv1connect.ClusterServiceClient) *outputPusher {
	return &outputPusher{
		eng:     eng,
		cluster: cluster,
		streams: map[string]*outputStream{},
		buf:     make([]byte, outputPushBatchBytes),
	}
}

// hasActiveWork reports whether any tracked command is still running or has
// pending output/EOF to push.
func (p *outputPusher) hasActiveWork() bool {
	targets := p.eng.outputTargets()
	for _, t := range targets {
		st := p.streams[t.id]
		if !t.ended || st == nil || !st.eofSent {
			return true
		}
	}
	return false
}

// once scans active/recently-ended commands and pushes up to one batch per
// command with new output or a pending EOF. Returns more=true if any command
// still has unread bytes remaining on disk.
func (p *outputPusher) once(ctx context.Context) (more bool, err error) {
	targets := p.eng.outputTargets()
	if len(targets) == 0 {
		clear(p.streams)
		return false, nil
	}
	active := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		active[t.id] = struct{}{}
	}
	for id := range p.streams {
		if _, ok := active[id]; !ok {
			delete(p.streams, id)
		}
	}

	for _, t := range targets {
		st := p.streams[t.id]
		if st == nil {
			st = &outputStream{}
			p.streams[t.id] = st
		}
		if st.eofSent {
			continue
		}
		cmdMore, err := p.pushCommand(ctx, t, st)
		if err != nil {
			return false, err
		}
		if cmdMore {
			more = true
		}
	}
	return more, nil
}

func (p *outputPusher) pushCommand(ctx context.Context, t outputTarget, st *outputStream) (more bool, err error) {
	var chunks []*pb.CommandOutputChunk
	var fileSize int64
	offset := st.offset

	f, err := os.Open(t.logPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Command has written no output file yet.
	case err != nil:
		return false, err
	default:
		defer f.Close()
		if info, serr := f.Stat(); serr == nil {
			fileSize = info.Size()
		}
		data, next, rerr := cmdlog.ReadChunk(f, t.logPath, offset, p.buf)
		if rerr != nil {
			return false, rerr
		}
		if len(data) > 0 {
			if next == offset+int64(len(data)) {
				if st.tsIdx == nil || next > st.tsIdx.MaxOffset() {
					st.tsIdx, _ = cmdlog.ReadTimestamps(t.logPath)
				}
				if !t.ended {
					// Every recorder writes a byte before its timestamp
					// (TimestampWriter, the host's RecordIfGrown poller):
					// bytes past the index wait for theirs, as CommandDO
					// keeps a chunk's timestamp for good. Once it ended,
					// its output was read to the end and stamped
					// (finishOutput), so what is left goes as it is.
					covered := max(0, st.tsIdx.MaxOffset()-offset)
					data = data[:min(int64(len(data)), covered)]
					next = offset + int64(len(data))
				}
				_ = st.tsIdx.SplitChunks(offset, data, func(chunkOff int64, chunk []byte, ts int64) error {
					cp := append([]byte(nil), chunk...)
					chunks = append(chunks, &pb.CommandOutputChunk{
						Offset:     chunkOff,
						NextOffset: chunkOff + int64(len(chunk)),
						ReceivedAt: ts,
						Data:       cp,
					})
					return nil
				})
				if len(chunks) > outputPushMaxChunks {
					// Keep prefix that fits within chunk count limit; next pass reads the rest.
					chunks = chunks[:outputPushMaxChunks]
					next = chunks[len(chunks)-1].NextOffset
				}
			} else {
				cp := append([]byte(nil), data...)
				chunks = append(chunks, &pb.CommandOutputChunk{
					Offset:     offset,
					NextOffset: next,
					ReceivedAt: 0,
					Data:       cp,
				})
			}
			offset = next
		}
	}

	reachedEOF := offset >= fileSize
	sendEOF := t.ended && reachedEOF
	if len(chunks) == 0 && !sendEOF {
		return false, nil
	}

	rpcCtx, cancel := context.WithTimeout(ctx, outputPushRPCTimeout)
	defer cancel()
	resp, err := p.cluster.PushCommandOutput(rpcCtx, &pb.PushCommandOutputRequest{
		CommandId: t.id,
		Chunks:    chunks,
		Eof:       sendEOF,
	})
	if err != nil {
		if connect.CodeOf(err) == connect.CodeUnimplemented {
			if !p.unsupported {
				slog.Info("controller does not support PushCommandOutput yet; skipping live output push")
				p.unsupported = true
			}
			return false, nil
		}
		return false, err
	}
	p.unsupported = false
	if ack := resp.GetAckedOffset(); ack > offset {
		offset = ack
	}
	st.offset = offset
	if sendEOF {
		st.eofSent = true
		st.tsIdx = nil
	}
	return !reachedEOF, nil
}

// runOutputPusher runs p until ctx ends: pushing output within
// outputPushPollInterval while commands are active (immediately while more
// bytes remain or on kick), and idling at ReportInterval when no commands are
// running.
func runOutputPusher(ctx context.Context, p *outputPusher, kicked, wake <-chan struct{}, onFail func(error)) error {
	failures := 0
	for {
		if wake != nil {
			select {
			case <-wake:
			default:
			}
		}
		more, err := p.once(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			e := api.FromConnect(err)
			switch {
			case e.Code == api.CodeNotJoined || e.Code == api.CodeClusterMismatch:
				return e
			case failures == 0 || failures%10 == 0:
				slog.Warn("push output failed; retrying", "err", e, "failures", failures+1)
			}
			if onFail != nil {
				onFail(err)
			}
			failures++
			wait := min(time.Second<<min(failures-1, 6), maxRetryWait)
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-wake:
				timer.Stop()
			case <-timer.C:
			}
			continue
		}
		failures = 0
		if more {
			continue
		}
		wait := api.ReportInterval
		if p.hasActiveWork() && !p.unsupported {
			wait = outputPushPollInterval
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-kicked:
			timer.Stop()
		case <-wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}
