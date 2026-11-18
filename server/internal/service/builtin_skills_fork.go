// Added by the mouse9527/multica fork: built-in skills behind fork feature
// flags (M-150).

package service

import (
	"context"

	"github.com/multica-ai/multica/server/internal/featureflags"
)

// RouterSetupSkillName is the built-in skill that teaches an agent to set up
// and run a Super Agent (router) from its chat. It ships with the binary so a
// workspace never has to import or attach it, and a release updates it.
const RouterSetupSkillName = "multica-router-setup"

// forkBuiltinSkillGates names the fork built-ins that ship only while a fork
// feature is on. With the feature off they are not handed out at all, so the
// skill set an agent receives is upstream's.
var forkBuiltinSkillGates = map[string]func(context.Context, *TaskService) bool{
	RouterSetupSkillName: func(ctx context.Context, s *TaskService) bool {
		return featureflags.RouterV1Enabled(ctx, s.FeatureFlags)
	},
}

// withoutGatedForkSkills drops every fork built-in whose feature is off.
//
// BuiltinSkills carries no request context, and server feature flags are
// process-wide (no claim path sets an evaluation context), so a background
// context evaluates them exactly as a request would.
func (s *TaskService) withoutGatedForkSkills(skills []AgentSkillData) []AgentSkillData {
	ctx := context.Background()
	kept := make([]AgentSkillData, 0, len(skills))
	for _, skill := range skills {
		if gate, ok := forkBuiltinSkillGates[skill.Name]; ok && !gate(ctx, s) {
			continue
		}
		kept = append(kept, skill)
	}
	return kept
}
