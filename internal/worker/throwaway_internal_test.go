package worker

import (
	"testing"
	"time"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/tailnet"
)

// A step releases a throwaway workspace once it is done with, whatever
// came before (a worker restart that cut its release short, a client that
// gave up on it): its command ended, or was forgotten as long ended, or no
// command came for it within the grace. It keeps one whose command runs,
// or that still waits, within the grace, for its command.
func TestStepReleasesAThrowawayOnceItIsDoneWith(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ran     string          // its throwaway command
		state   pb.CommandState // that command's, when the worker still has it
		idle    time.Duration   // since it was last active
		release bool
	}{
		{name: "its command ended", ran: "ran", state: succeeded, release: true},
		{name: "its command was forgotten", ran: "gone", release: true},
		{name: "no command came within the grace", idle: unusedThrowawayGrace + time.Second, release: true},
		{name: "its command runs", ran: "live", state: running, idle: time.Hour},
		{name: "it waits for its command", idle: unusedThrowawayGrace - time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			id := staticID{peer: tailnet.Peer{NodeID: "nLaptop", Hostname: "laptop", Login: "ramon@"}, uid: 1000}
			eng, _, wsID := startShellTestEngine(t, id, &now)
			at := now.UnixMilli()
			eng.mu.Lock()
			w := eng.workspaces[wsID]
			w.ws().Throwaway = true
			w.ws().LastActiveAt = now.Add(-tc.idle).UnixMilli()
			w.rec.ThrowawayCommand = tc.ran
			if tc.state != pb.CommandState_COMMAND_STATE_UNSPECIFIED {
				cmd := &pb.Command{Id: tc.ran, WorkspaceId: wsID, NodeId: "n1", State: tc.state, QueuedAt: at}
				if tc.state == succeeded {
					cmd.EndedAt = &at
				}
				eng.commands[tc.ran] = eng.newCommand(&pb.CommandRecord{Command: cmd})
			}
			eng.mu.Unlock()

			eng.releaseSpentThrowaways()
			if !tc.release {
				eng.mu.Lock()
				started := w.releaseQueued || w.releasing || w.ws().State == released
				eng.mu.Unlock()
				if started {
					t.Fatalf("throwaway %s is being released; want it kept", wsID)
				}
				return
			}
			deadline := time.Now().Add(30 * time.Second)
			for {
				ws, err := eng.Workspace(wsID)
				if err != nil {
					t.Fatal(err)
				}
				if ws.State == released {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("throwaway %s = %s; want it released", wsID, ws.State)
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}
