package auth

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestCodexPreferHigherPriorityReturnsSessionsAfterRecovery(t *testing.T) {
	for _, tc := range []struct {
		provider string
		enabled  bool
	}{
		{provider: "codex", enabled: true},
		{provider: "codex", enabled: false},
		{provider: "claude", enabled: true},
	} {
		for _, mixed := range []bool{false, true} {
			for _, lcp := range []bool{false, true} {
				for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
					t.Run(fmt.Sprintf("%s/enabled=%v/mixed=%v/lcp=%v/status=%d", tc.provider, tc.enabled, mixed, lcp, status), func(t *testing.T) {
						ctx := context.Background()
						affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
							Fallback: &FillFirstSelector{}, CodexPreferHigherPriority: tc.enabled,
						})
						t.Cleanup(affinity.Stop)
						manager := NewManager(nil, affinity, nil)
						manager.RegisterExecutor(schedulerTestExecutor{provider: tc.provider})
						const model = "priority-recovery"
						for _, auth := range []*Auth{
							{ID: "included", Provider: tc.provider, Status: StatusActive, Attributes: map[string]string{"priority": "100"}},
							{ID: "credits", Provider: tc.provider, Status: StatusActive, Attributes: map[string]string{"priority": "0"}},
						} {
							if _, err := manager.Register(WithSkipPersist(ctx), auth); err != nil {
								t.Fatal(err)
							}
							registry.GetGlobalRegistry().RegisterClient(auth.ID, tc.provider, []*registry.ModelInfo{{ID: model}})
							t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
						}
						options := func() cliproxyexecutor.Options {
							if lcp {
								return cliproxyexecutor.Options{
									SourceFormat:    sdktranslator.FormatOpenAI,
									OriginalRequest: []byte(`{"messages":[{"role":"user","content":"priority recovery"}]}`),
									Metadata:        map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "priority-recovery"},
								}
							}
							return cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"priority-recovery"}}}
						}
						pick := func(want string) {
							t.Helper()
							var auth *Auth
							var err error
							if mixed {
								auth, _, _, err = manager.pickNextMixed(ctx, []string{tc.provider}, model, options(), nil)
							} else {
								auth, _, err = manager.pickNext(ctx, tc.provider, model, options(), nil)
							}
							if err != nil || auth == nil || auth.ID != want {
								t.Fatalf("pick = %v, %v; want %s", auth, err, want)
							}
						}
						pick("included")
						manager.MarkResult(ctx, Result{AuthID: "included", Provider: tc.provider, Model: model, Error: &Error{HTTPStatus: status, Message: "unavailable"}})
						pick("credits")
						expireSessionAffinityPriorityModelCooldown(t, manager, "included", model)
						want := "credits"
						if tc.provider == "codex" && tc.enabled {
							want = "included"
						}
						pick(want)
					})
				}
			}
		}
	}
}

func TestCodexPreferHigherPriorityHonorsNeverRebind(t *testing.T) {
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &FillFirstSelector{}, RebindOn: SessionAffinityRebindOnNever, CodexPreferHigherPriority: true,
	})
	t.Cleanup(selector.Stop)
	low := &Auth{ID: "low", Provider: "codex", Status: StatusActive}
	high := &Auth{ID: "high", Provider: "codex", Status: StatusActive, Attributes: map[string]string{"priority": "100"}}
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"never-rebind"}}}
	if _, err := selector.Pick(context.Background(), "codex", "", opts, []*Auth{low}); err != nil {
		t.Fatal(err)
	}
	got, err := selector.Pick(context.Background(), "codex", "", opts, []*Auth{low, high})
	if err != nil || got == nil || got.ID != "low" {
		t.Fatalf("never-rebind moved the existing session: %v, %v", got, err)
	}
}
