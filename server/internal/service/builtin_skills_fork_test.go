// Added by the mouse9527/multica fork: built-in skills behind fork feature
// flags (M-150).

package service

import (
	"context"
	"os"
	"path"
	"testing"

	"github.com/multica-ai/multica/server/internal/featureflags"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/featureflag"
)

func routerV1TestFlags(enabled bool) *featureflag.Service {
	provider := featureflag.NewStaticProvider()
	provider.Set(featureflags.RouterV1, featureflag.Rule{Default: enabled})
	return featureflag.NewService(provider)
}

// A workspace that never imported router-setup still gets it on Mika's claim,
// with no attach step, as long as router_v1 is on.
func TestRouterSetupSkillShipsToMikaWithRouterV1(t *testing.T) {
	svc := &TaskService{FeatureFlags: routerV1TestFlags(true)}

	for _, legacy := range []bool{false, true} {
		mika := svc.BuiltinSkills(MikaSystemKey, legacy)
		if !named(mika, RouterSetupSkillName) {
			t.Errorf("legacy=%v: Mika does not receive %q with router_v1 on", legacy, RouterSetupSkillName)
		}
		for _, other := range []string{PlatformSkillName, "multica-onboarding"} {
			if !named(mika, other) {
				t.Errorf("legacy=%v: Mika lost %q; the gate must only drop gated skills", legacy, other)
			}
		}
	}

	for _, key := range []string{"", EvolutionSystemKey} {
		if named(svc.BuiltinSkills(key, false), RouterSetupSkillName) {
			t.Errorf("agent with system key %q receives %q; it is scoped to Mika", key, RouterSetupSkillName)
		}
	}
}

// With router_v1 off (or no flag service at all) the skill set is upstream's.
func TestRouterSetupSkillIsNotShippedWithRouterV1Off(t *testing.T) {
	for name, svc := range map[string]*TaskService{
		"router_v1 off": {FeatureFlags: routerV1TestFlags(false)},
		"no flags":      {},
	} {
		mika := svc.BuiltinSkills(MikaSystemKey, false)
		if named(mika, RouterSetupSkillName) {
			t.Errorf("%s: Mika receives %q", name, RouterSetupSkillName)
		}
		if !named(mika, PlatformSkillName) || !named(mika, "multica-onboarding") {
			t.Errorf("%s: Mika lost an upstream built-in: %v", name, skillNames(mika))
		}
	}
}

// The slim claim path builds its refs from the same set, so the daemon is told
// about the skill exactly when the full claim would ship it.
func TestRouterSetupSkillRefFollowsTheGate(t *testing.T) {
	for _, on := range []bool{true, false} {
		svc := &TaskService{Queries: db.New(&skillReadDBTX{}), FeatureFlags: routerV1TestFlags(on)}
		_, refs, err := svc.LoadAgentSkillBundles(context.Background(), testUUID(9), MikaSystemKey, false)
		if err != nil {
			t.Fatalf("router_v1=%v: LoadAgentSkillBundles: %v", on, err)
		}
		got := false
		for _, ref := range refs {
			if ref.ID == BuiltinSkillID(RouterSetupSkillName) {
				got = true
			}
		}
		if got != on {
			t.Errorf("router_v1=%v: ref for %q present = %v", on, RouterSetupSkillName, got)
		}
	}
}

// The embedded file is the only source: what the claim ships is byte for byte
// the SKILL.md in this package, so editing it and releasing is the whole update.
func TestRouterSetupSkillShipsTheEmbeddedFile(t *testing.T) {
	onDisk, err := os.ReadFile(path.Join(builtinSkillsRoot, RouterSetupSkillName, "SKILL.md"))
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	svc := &TaskService{FeatureFlags: routerV1TestFlags(true)}
	for _, skill := range svc.BuiltinSkills(MikaSystemKey, false) {
		if skill.Name == RouterSetupSkillName {
			if skill.Content != string(onDisk) {
				t.Fatal("shipped content differs from the embedded SKILL.md")
			}
			return
		}
	}
	t.Fatalf("%q not shipped", RouterSetupSkillName)
}

func skillNames(skills []AgentSkillData) []string {
	names := make([]string, 0, len(skills))
	for _, s := range skills {
		names = append(names, s.Name)
	}
	return names
}
