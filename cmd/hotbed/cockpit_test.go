package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// Every message with a cockpit_url has a page, so no output leaves one empty
// (or panics filling it); and those are exactly the cockpit's pages.
func TestEveryCockpitURLHasAPage(t *testing.T) {
	c := cockpit{base: "http://ubu1:7120"}
	var with []string
	var visit func(protoreflect.MessageDescriptors)
	visit = func(ms protoreflect.MessageDescriptors) {
		for i := 0; i < ms.Len(); i++ {
			md := ms.Get(i)
			visit(md.Messages())
			if md.Fields().ByName("cockpit_url") == nil {
				continue
			}
			with = append(with, string(md.Name()))
			mt, err := protoregistry.GlobalTypes.FindMessageByName(md.FullName())
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := c.pageOf(mt.New()); !ok {
				t.Errorf("%s has a cockpit_url but no page", md.FullName())
			}
		}
	}
	visit(pb.File_hotbed_v1_hotbed_proto.Messages())
	slices.Sort(with)
	if want := []string{"Command", "FinishedCommand", "Memory", "Node", "Question", "Session", "SessionWorkspace", "Workspace"}; !slices.Equal(with, want) {
		t.Errorf("messages with a cockpit_url = %v, want %v", with, want)
	}
	if got := c.node("asahi mbp"); got != "http://ubu1:7120/nodes/asahi%20mbp" {
		t.Errorf("a node's page = %q, want its name escaped", got)
	}
}

// cockpit is the cockpit of the tests' controller, as the CLI makes its URLs.
func (c *cluster) cockpit() cockpit {
	c.t.Helper()
	addr, err := c.cli.controller()
	if err != nil {
		c.t.Fatal(err)
	}
	cp, err := newCockpit(addr)
	if err != nil {
		c.t.Fatal(err)
	}
	return cp
}

// Each session, workspace, command and node a command prints carries its page
// on the controller the CLI called, however deep in the output it is.
// A command made from the API's descriptors prints messages of no Go type,
// which carry their pages too.
func TestDescriptorMessagesCarryCockpitURLs(t *testing.T) {
	s, err := apiSchema()
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Files.FindDescriptorByName(fullName(&pb.GetSessionResponse{}))
	if err != nil {
		t.Fatal(err)
	}
	res := dynamicpb.NewMessage(d.(protoreflect.MessageDescriptor))
	sess := res.Mutable(res.Descriptor().Fields().ByName("session")).Message()
	sess.Set(sess.Descriptor().Fields().ByName("id"), protoreflect.ValueOfString("brave-otter"))
	cockpit{base: "http://ubu1:7120"}.fill(res)
	if got := sess.Get(sess.Descriptor().Fields().ByName("cockpit_url")).String(); got != "http://ubu1:7120/sessions/brave-otter" {
		t.Errorf("cockpit_url = %q", got)
	}
}

func TestOutputsCarryCockpitURLs(t *testing.T) {
	c := newCluster(t)
	// Spelled out rather than made by c.cockpit(), to check what it makes.
	addr, err := c.cli.controller()
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + addr
	c.addRepo(upstream(t))

	const S = "calm-owl"
	sess := &pb.Session{}
	c.ok(sess, "session", "start", S)
	if sess.CockpitUrl != base+"/sessions/calm-owl" {
		t.Errorf("session start: cockpitUrl %q", sess.CockpitUrl)
	}
	c.workspace(S, nil)
	ws := &pb.Workspace{}
	c.ok(ws, "workspace", "show", "-s", S)
	if ws.CockpitUrl != base+"/workspaces/"+ws.Id {
		t.Errorf("workspace show: cockpitUrl %q", ws.CockpitUrl)
	}
	cmd := &pb.Command{}
	c.ok(cmd, "run", "-s", S, "-r", "app", "--detach", "--port", "5173", "--", "true")
	if cmd.CockpitUrl != base+"/commands/"+cmd.Id || cmd.Url != "http://asahimbp:5173" {
		t.Errorf("run --detach: cockpitUrl %q, url %q (the server's, untouched)", cmd.CockpitUrl, cmd.Url)
	}
	c.eventually("the command ends", func() bool {
		var st sessionStatusResult
		c.ok(&st, "session", "status", "-s", S)
		return len(st.Commands) == 1 && st.Commands[0].State == "succeeded"
	})

	ps := &pb.ListCommandsResponse{}
	c.ok(ps, "ps", "-s", S)
	if len(ps.Commands) != 1 || ps.Commands[0].CockpitUrl != base+"/commands/"+cmd.Id {
		t.Errorf("ps = %v", ps.Commands)
	}
	nodes := &pb.ListNodesResponse{}
	c.ok(nodes, "nodes")
	if len(nodes.Nodes) != 1 || nodes.Nodes[0].CockpitUrl != base+"/nodes/asahimbp" {
		t.Errorf("nodes = %v", nodes.Nodes)
	}
	ls := &pb.ListSessionsResponse{}
	c.ok(ls, "session", "ls", "-s", S)
	if len(ls.Sessions) != 1 || ls.Sessions[0].CockpitUrl != base+"/sessions/calm-owl" ||
		len(ls.Sessions[0].Workspaces) != 1 || ls.Sessions[0].Workspaces[0].CockpitUrl != base+"/workspaces/"+ws.Id {
		t.Errorf("session ls = %v", ls.Sessions)
	}

	var st sessionStatusResult
	c.ok(&st, "session", "status", "-s", S)
	if st.CockpitURL != base+"/sessions/calm-owl" ||
		len(st.Workspaces) != 1 || st.Workspaces[0].CockpitURL != base+"/workspaces/"+ws.Id ||
		len(st.Commands) != 1 || st.Commands[0].CockpitURL != base+"/commands/"+cmd.Id {
		t.Errorf("session status = %+v", st)
	}

	snap := &pb.Snapshot{}
	c.ok(snap, "snapshot")
	if len(snap.Sessions) != 1 || snap.Sessions[0].CockpitUrl != base+"/sessions/calm-owl" ||
		len(snap.Workspaces) != 1 || snap.Workspaces[0].CockpitUrl != base+"/workspaces/"+ws.Id ||
		len(snap.Nodes) != 1 || snap.Nodes[0].CockpitUrl != base+"/nodes/asahimbp" {
		t.Errorf("snapshot sessions %v, workspaces %v, nodes %v", snap.Sessions, snap.Workspaces, snap.Nodes)
	}

	// A question links to its own page, and question add prints the link.
	added := &pb.AddQuestionsResponse{}
	c.okIn(storeQuestion, added, "question", "add", "-s", S)
	qURL := fmt.Sprintf("%s/questions/%d", base, added.Questions[0].GetId())
	if len(added.Questions) != 1 || added.Questions[0].CockpitUrl != qURL {
		t.Errorf("question add = %v, want it to link to %s", added.Questions, qURL)
	}
	shown := &pb.Question{}
	c.ok(shown, "question", "show", fmt.Sprint(added.Questions[0].GetId()))
	if shown.CockpitUrl != qURL {
		t.Errorf("question show: cockpitUrl %q, want %s", shown.CockpitUrl, qURL)
	}
	code, text, stderr := c.hotbedIn(storeQuestion, "question", "add", "-s", S)
	if want := fmt.Sprintf("%s/questions/%d\n", base, added.Questions[0].GetId()+1); code != exitOK || !strings.Contains(text, "\n      "+want) {
		t.Errorf("question add printed %q (stderr %q, exit %d), want the question's link %q on its own line", text, stderr, code, want)
	}

	var released releaseResult
	c.ok(&released, "workspace", "release", "-s", S)
	if released.CockpitURL != base+"/workspaces/"+ws.Id {
		t.Errorf("workspace release = %+v", released)
	}
	var ui uiResult
	c.ok(&ui, "ui", "--no-open")
	if ui.URL != base+"/" {
		t.Errorf("ui = %+v", ui)
	}
}
