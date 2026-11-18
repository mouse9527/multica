// Added by the mouse9527/multica fork: Mika's pointer to the Super Agent guide (M-150).

package service

import (
	"context"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/pkg/featureflag"
)

func routerV1TestFlags(enabled bool) *featureflag.Service {
	provider := featureflag.NewStaticProvider()
	provider.Set(featureflags.RouterV1, featureflag.Rule{Default: enabled})
	return featureflag.NewService(provider)
}

// With router_v1 on, Mika's prompt carries one pointer to the guide, after the
// product-owned half and before the workspace notes, which must stay last.
func TestMikaRouterGuidePointerWithRouterV1(t *testing.T) {
	svc := &TaskService{FeatureFlags: routerV1TestFlags(true)}
	ctx := context.Background()
	system := MikaSystemInstructions("Mika")

	got := svc.WithMikaRouterGuide(ctx, MikaSystemKey, "Mika", ComposeMikaInstructions("Mika", ""))
	if got != system+"\n\n"+mikaRouterGuidePointer {
		t.Errorf("no notes: got\n%s", got)
	}
	if !strings.Contains(mikaRouterGuidePointer, "`multica router guide`") {
		t.Error("the pointer must name `multica router guide`")
	}

	const notes = "Our main repo is acme/platform."
	got = svc.WithMikaRouterGuide(ctx, MikaSystemKey, "Mika", ComposeMikaInstructions("Mika", notes))
	pointer := strings.Index(got, mikaRouterGuidePointer)
	section := strings.Index(got, mikaWorkspaceNotesSection)
	if !strings.HasPrefix(got, system) || pointer < 0 || section < pointer || !strings.HasSuffix(got, notes) {
		t.Errorf("with notes: pointer must sit between the system half and the notes, got\n%s", got)
	}
	if strings.Count(got, "multica router guide") != 1 {
		t.Error("the pointer must appear once")
	}
}

// With router_v1 off (or no flag service), and for every other agent, the
// prompt is exactly upstream's.
func TestMikaRouterGuidePointerOtherwiseUnchanged(t *testing.T) {
	ctx := context.Background()
	mika := ComposeMikaInstructions("Mika", "notes")
	for name, svc := range map[string]*TaskService{
		"router_v1 off": {FeatureFlags: routerV1TestFlags(false)},
		"no flags":      {},
	} {
		if got := svc.WithMikaRouterGuide(ctx, MikaSystemKey, "Mika", mika); got != mika {
			t.Errorf("%s: Mika's prompt changed:\n%s", name, got)
		}
	}
	on := &TaskService{FeatureFlags: routerV1TestFlags(true)}
	for _, key := range []string{"", EvolutionSystemKey} {
		if got := on.WithMikaRouterGuide(ctx, key, "x", "own instructions"); got != "own instructions" {
			t.Errorf("system key %q: instructions changed to %q", key, got)
		}
	}
}
