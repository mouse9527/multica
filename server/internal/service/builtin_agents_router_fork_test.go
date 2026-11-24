// Added by the mouse9527/multica fork: Mika's Super Agent setup pointer (M-150, chat layer since M-250).

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

// With router_v1 on, Mika's chats get one pointer to the guide.
func TestSuperAgentSetupPointerForMika(t *testing.T) {
	svc := &TaskService{FeatureFlags: routerV1TestFlags(true)}
	got := svc.SuperAgentSetupPointer(context.Background(), MikaSystemKey)
	if got != mikaRouterSetupPointer {
		t.Fatalf("got %q", got)
	}
	if !strings.HasPrefix(got, "## Super Agent\n\n") || strings.Count(got, "`multica router guide`") != 1 {
		t.Errorf("the pointer must be one section naming `multica router guide` once:\n%s", got)
	}
}

// With router_v1 off (or no flag service), and for every other agent, there is
// no pointer.
func TestSuperAgentSetupPointerOtherwiseEmpty(t *testing.T) {
	ctx := context.Background()
	for name, svc := range map[string]*TaskService{
		"router_v1 off": {FeatureFlags: routerV1TestFlags(false)},
		"no flags":      {},
	} {
		if got := svc.SuperAgentSetupPointer(ctx, MikaSystemKey); got != "" {
			t.Errorf("%s: got %q", name, got)
		}
	}
	on := &TaskService{FeatureFlags: routerV1TestFlags(true)}
	for _, key := range []string{"", EvolutionSystemKey} {
		if got := on.SuperAgentSetupPointer(ctx, key); got != "" {
			t.Errorf("system key %q: got %q", key, got)
		}
	}
}
