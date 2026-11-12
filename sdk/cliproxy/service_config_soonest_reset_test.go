package cliproxy

import (
	"context"
	"net/http"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestSoonestResetRoutingSelector(t *testing.T) {
	state := normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "Soonest-Reset"},
	})
	if state.strategy != "soonest-reset" {
		t.Fatalf("strategy = %q, want soonest-reset", state.strategy)
	}
	if _, ok := newRoutingSelector(state).(*coreauth.SoonestResetSelector); !ok {
		t.Fatalf("selector type = %T, want *auth.SoonestResetSelector", newRoutingSelector(state))
	}
}

func TestCodexPriorityConfigControlsExistingSessionSelection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		rebind  string
		want    string
	}{
		{name: "default preserves binding", want: "credits"},
		{name: "enabled returns to included allowance", enabled: true, want: "included"},
		{name: "never preserves binding", enabled: true, rebind: "never", want: "credits"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := normalizedRoutingRuntimeState(&internalconfig.Config{Routing: internalconfig.RoutingConfig{
				SessionAffinity: true, CodexPreferHigherPriority: tc.enabled, SessionAffinityRebindOn: tc.rebind,
			}})
			selector := newRoutingSelector(state)
			t.Cleanup(selector.(*coreauth.SessionAffinitySelector).Stop)
			low := &coreauth.Auth{ID: "credits", Provider: "codex", Status: coreauth.StatusActive}
			high := &coreauth.Auth{ID: "included", Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"priority": "100"}}
			opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"config-priority"}}}
			if _, err := selector.Pick(context.Background(), "codex", "", opts, []*coreauth.Auth{low}); err != nil {
				t.Fatal(err)
			}
			got, err := selector.Pick(context.Background(), "codex", "", opts, []*coreauth.Auth{low, high})
			if err != nil || got == nil || got.ID != tc.want {
				t.Fatalf("selection = %v, %v; want %s", got, err, tc.want)
			}
		})
	}
}
