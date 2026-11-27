package main

import (
	"testing"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// The briefing names each skill and memory by the ref its get command takes,
// with no scope: that is for the get call to say. Only names that clash get
// their scope, so the ref still reads the one meant.
func TestBriefingNamesScopeOnlyWhenNamesClash(t *testing.T) {
	sess := &pb.Session{
		Skills: &pb.SkillBriefing{Skills: []*pb.SkillDescriptor{
			{Name: "hotbed", Scope: pb.SkillScope_SKILL_SCOPE_BUILTIN, Description: "Built in."},
			{Name: "hotbed", Scope: pb.SkillScope_SKILL_SCOPE_PROJECT, Project: "app", Description: "Stored under a built-in's name."},
			{Name: "deploy", Scope: pb.SkillScope_SKILL_SCOPE_PROJECT, Project: "app", Repo: "github.com/org/app", Description: "The project's."},
			{Name: "deploy", Scope: pb.SkillScope_SKILL_SCOPE_GLOBAL, Description: "Everyone's."},
			{Name: "lint", Scope: pb.SkillScope_SKILL_SCOPE_GLOBAL, Description: "Unique."},
		}},
		Memories: &pb.MemoryBriefing{Entries: []*pb.MemoryIndexEntry{
			{Slug: "ports", Project: "app", Repo: "github.com/org/app", Summary: "The project's ports."},
			{Slug: "ports", Summary: "Global ports."},
			{Slug: "short-replies", Summary: "Unique."},
		}},
	}
	want := "Skills (read one with `hotbed skills get NAME` when its description applies):\n" +
		"  hotbed — Built in.\n" +
		"  app/hotbed — Stored under a built-in's name.\n" +
		"  app/deploy — The project's.\n" +
		"  global/deploy — Everyone's.\n" +
		"  lint — Unique.\n" +
		"\n" +
		"Memories (read one with `hotbed memory get SLUG` when its summary applies):\n" +
		"  app/ports — The project's ports.\n" +
		"  global/ports — Global ports.\n" +
		"  short-replies — Unique."
	if got := formatBriefing(sess, cliBriefing); got != want {
		t.Errorf("briefing =\n%s\nwant\n%s", got, want)
	}
}
