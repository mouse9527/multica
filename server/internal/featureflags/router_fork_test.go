// Added by the mouse9527/multica fork: Router page settings (M-105).

package featureflags

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/pkg/featureflag"
)

// The web client shows the Router settings only when router_chat is in
// effect, which needs router_v1 and router_dispatch as well.
func TestFrontendRouterChatNeedsTheWholeChain(t *testing.T) {
	for _, tc := range []struct {
		v1, dispatch, chat, want bool
	}{
		{true, true, true, true},
		{true, true, false, false},
		{true, false, true, false},
		{false, true, true, false},
		{false, false, false, false},
	} {
		provider := featureflag.NewStaticProvider()
		provider.Set(RouterV1, featureflag.Rule{Default: tc.v1})
		provider.Set(RouterDispatch, featureflag.Rule{Default: tc.dispatch})
		provider.Set(RouterChat, featureflag.Rule{Default: tc.chat})
		got := EvaluateFrontendPublicFlags(context.Background(), featureflag.NewService(provider))
		if got[RouterChat] != tc.want {
			t.Errorf("v1=%v dispatch=%v chat=%v: router_chat = %v, want %v", tc.v1, tc.dispatch, tc.chat, got[RouterChat], tc.want)
		}
	}
}
