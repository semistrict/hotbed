package main

import (
	"fmt"
	"strings"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/skills"
)

// What a session start tells its agent: the skills it can load (hotbed's
// own first), the memories it can read, and the controller's reminders.
// Each skill and memory is one line saying when it applies; the agent reads
// the rest once one does, so the briefing stays small however much is kept.
// `hotbed session start` prints it, and `hotbed mcp` puts it in its
// instructions.

// sessionStarted is what `hotbed session start` returns: the session, which
// it embeds so --json writes it as the message it is, and whose plain text
// is the briefing (formatSessionStarted).
type sessionStarted struct{ *pb.Session }

// withBuiltinSkills puts the skills compiled into this CLI, which say how to
// use hotbed itself, ahead of the stored skills the controller briefed.
func withBuiltinSkills(sess *pb.Session) (*pb.Session, error) {
	list, err := skills.List()
	if err != nil {
		return nil, fmt.Errorf("read the built-in skills: %w", err)
	}
	b := &pb.SkillBriefing{}
	for _, s := range list {
		b.Skills = append(b.Skills, &pb.SkillDescriptor{
			Name:        s.Name,
			Scope:       pb.SkillScope_SKILL_SCOPE_BUILTIN,
			Description: s.Description,
			Uri:         builtinSkillURI(s.Name),
			FileCount:   int32(1 + len(s.References)),
		})
	}
	if stored := sess.GetSkills(); stored != nil {
		b.Skills = append(b.Skills, stored.Skills...)
		b.TotalActive = stored.TotalActive
	}
	sess.Skills = b
	return sess, nil
}

func builtinSkillURI(name string) string { return "skill://builtin/" + name + "/SKILL.md" }

// briefingHints says how the reader loads what the briefing lists.
type briefingHints struct {
	skill, memory, moreMemories string
}

var (
	cliBriefing = briefingHints{
		skill:        "`hotbed skills get NAME`",
		memory:       "`hotbed memory get SLUG`",
		moreMemories: "`hotbed memory ls`",
	}
	mcpBriefing = briefingHints{
		skill:        "`resources/read` of skill://SCOPE/NAME/SKILL.md, or `hotbed skills get NAME`",
		memory:       "tool `memory_get`",
		moreMemories: "tool `memory_ls`",
	}
)

func formatSessionStarted(sess *pb.Session) string {
	var sb strings.Builder
	sb.WriteString(sess.Id)
	for _, kv := range [][2]string{{"project", sess.Project}, {"model", sess.Model}, {"t3", sess.T3CodeThreadId}} {
		if kv[1] != "" {
			fmt.Fprintf(&sb, "  %s=%s", kv[0], kv[1])
		}
	}
	sb.WriteByte('\n')
	if body := formatBriefing(sess, cliBriefing); body != "" {
		sb.WriteString("\n" + body + "\n")
	}
	return sb.String()
}

// formatBriefing renders the session's skills, memories and reminder, each
// part only when it has something to say.
func formatBriefing(sess *pb.Session, h briefingHints) string {
	var parts []string
	if sk := sess.GetSkills().GetSkills(); len(sk) > 0 {
		var sb strings.Builder
		fmt.Fprintf(&sb, "Skills (read one with %s when its description applies):", h.skill)
		for _, s := range sk {
			fmt.Fprintf(&sb, "\n  [%s] %s — %s", skillScopeTag(s), s.Name, s.Description)
		}
		parts = append(parts, sb.String())
	}
	if m := sess.GetMemories(); len(m.GetEntries()) > 0 || m.GetOmitted() > 0 {
		var sb strings.Builder
		fmt.Fprintf(&sb, "Memories (read one with %s when its summary applies):", h.memory)
		for _, e := range m.Entries {
			fmt.Fprintf(&sb, "\n  [%s] %s — %s", scopeTag(e.Project, e.Repo), e.Slug, e.Summary)
		}
		if m.Omitted > 0 {
			fmt.Fprintf(&sb, "\n  (+%d more, left out for room: %s)", m.Omitted, h.moreMemories)
		}
		parts = append(parts, sb.String())
	}
	if r := sess.GetReminder(); r != "" {
		parts = append(parts, r)
	}
	return strings.Join(parts, "\n\n")
}

func skillScopeTag(s *pb.SkillDescriptor) string {
	if s.Scope == pb.SkillScope_SKILL_SCOPE_BUILTIN {
		return "builtin"
	}
	return scopeTag(s.Project, s.Repo)
}

// scopeTag names where a stored skill or memory applies: global, or its
// project and, when it has one, the repo within it.
func scopeTag(project, repo string) string {
	if project == "" {
		return "global"
	}
	if repo != "" {
		return project + "/" + repo
	}
	return project
}
