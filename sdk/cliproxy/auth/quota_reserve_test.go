package auth

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestQuotaReserveEligibility(t *testing.T) {
	now := time.Unix(1790000000, 0)
	for _, tc := range []struct {
		name, used, after string
		reserve           any
		secondary         bool
		blocked           bool
	}{
		{name: "above reserve", reserve: 20, used: "79.99", after: "3600"},
		{name: "at reserve", reserve: 20, used: "80", after: "3600", blocked: true},
		{name: "below reserve", reserve: "20", used: "90", after: "3600", blocked: true},
		{name: "expired window", reserve: 20, used: "90", after: "0"},
		{name: "weekly secondary", reserve: 20, used: "80", after: "86400", secondary: true, blocked: true},
		{name: "missing reset protects known usage", reserve: 20, used: "80", blocked: true},
		{name: "unknown usage can learn", reserve: 20, after: "3600"},
		{name: "invalid usage can learn", reserve: 20, used: "NaN", after: "3600"},
		{name: "default disabled", used: "100", after: "3600"},
		{name: "zero disabled", reserve: 0, used: "100", after: "3600"},
		{name: "full reserve", reserve: 100, blocked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefix := "X-Codex-Primary-"
			if tc.secondary {
				prefix = "X-Codex-Secondary-"
			}
			auth := &Auth{Provider: "codex", Metadata: map[string]any{MetadataQuotaReservePercent: tc.reserve}, Quota: QuotaState{
				ObservedAt: now, Signals: map[string]string{prefix + "Used-Percent": tc.used, prefix + "Reset-After-Seconds": tc.after},
			}}
			_, blocked := codexQuotaReserveBlocks(auth, now)
			if blocked != tc.blocked {
				t.Fatalf("blocked = %v, want %v", blocked, tc.blocked)
			}
		})
	}
}

func TestQuotaReserveRejectsInvalidSettings(t *testing.T) {
	for _, raw := range []any{-1, 101, math.NaN(), math.Inf(1), true, "invalid", map[string]any{"percent": 20}} {
		auth := &Auth{ID: "invalid-reserve", Provider: "codex", Metadata: map[string]any{MetadataQuotaReservePercent: raw}}
		manager := NewManager(nil, nil, nil)
		if _, err := manager.Register(context.Background(), auth); err == nil {
			t.Fatalf("Register accepted reserve %v", raw)
		}
	}
}

func TestQuotaReserveWaitsForAllBlockingWindows(t *testing.T) {
	now := time.Unix(1790000000, 0)
	auth := &Auth{Provider: "codex", Metadata: map[string]any{MetadataQuotaReservePercent: 20}, Quota: QuotaState{
		ObservedAt: now, Signals: map[string]string{
			"X-Codex-Primary-Used-Percent": "80", "X-Codex-Primary-Reset-After-Seconds": "3600",
			"X-Codex-Secondary-Used-Percent": "90", "X-Codex-Secondary-Reset-After-Seconds": "86400",
		},
	}}
	for _, elapsed := range []time.Duration{0, time.Hour} {
		reset, blocked := codexQuotaReserveBlocks(auth, now.Add(elapsed))
		if !blocked || !reset.Equal(now.Add(24*time.Hour)) {
			t.Fatalf("after %v: blocked = %v, reset = %v", elapsed, blocked, reset)
		}
	}
	if _, blocked := codexQuotaReserveBlocks(auth, now.Add(24*time.Hour)); blocked {
		t.Fatal("reserve still blocks after both windows reset")
	}
}

func TestQuotaReserveSchedulerSynchronizesChangedDeadline(t *testing.T) {
	for _, selector := range []Selector{&RoundRobinSelector{}, &FillFirstSelector{}, &WeightedRoundRobinSelector{}} {
		t.Run(fmt.Sprintf("%T", selector), func(t *testing.T) {
			now := time.Unix(1790000000, 0)
			models := []string{"gpt-reserve-deadline-a", "gpt-reserve-deadline-b"}
			business := &Auth{ID: "reserve-deadline-business", Provider: "codex", Status: StatusActive,
				Attributes: map[string]string{"priority": "100"}, Metadata: map[string]any{MetadataQuotaReservePercent: 20},
				Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
					"X-Codex-Primary-Used-Percent": "85", "X-Codex-Primary-Reset-After-Seconds": "3600",
					"X-Codex-Secondary-Used-Percent": "79", "X-Codex-Secondary-Reset-After-Seconds": "86400",
				}},
			}
			credits := &Auth{ID: "reserve-deadline-credits", Provider: "codex", Status: StatusActive}
			scheduler := newAuthScheduler(selector)
			for _, auth := range []*Auth{business, credits} {
				registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: models[0]}, {ID: models[1]}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
				scheduler.upsertAuthLocked(auth, now)
			}
			provider := scheduler.providers["codex"]
			pick := func(at time.Time, want string) {
				t.Helper()
				for _, model := range models {
					shard := provider.ensureModelLocked(model, at)
					priority, ready := shard.highestReadyPriorityLocked(false, nil)
					if !ready {
						t.Fatalf("no ready credential for %s at %v", model, at)
					}
					auth := shard.pickReadyAtPriorityLocked(false, priority, scheduler.strategy, nil)
					if auth == nil || auth.ID != want {
						t.Fatalf("pick(%s) at %v = %v, want %s", model, at, auth, want)
					}
				}
			}
			pick(now, credits.ID)
			// An in-flight result for a adds a weekly block without changing the
			// credential's blocked status. Both cached shards need its deadline.
			updated := business.Clone()
			updated.Generation++
			updated.Quota.Signals["X-Codex-Secondary-Used-Percent"] = "81"
			scheduler.upsertAuthResultLocked(updated, []string{models[0]}, false, now.Add(10*time.Minute))
			pick(now.Add(2*time.Hour), credits.ID)
			pick(now.Add(24*time.Hour), business.ID)
		})
	}
}

func TestQuotaReserveRoutesAllModelsAndAffinityToFallback(t *testing.T) {
	for _, strategy := range []string{"round-robin", "weighted", "soonest-reset", "affinity", "affinity-quota", "affinity-never"} {
		t.Run(strategy, func(t *testing.T) {
			var selector Selector = &RoundRobinSelector{}
			switch strategy {
			case "weighted":
				selector = &WeightedRoundRobinSelector{}
			case "soonest-reset":
				selector = &SoonestResetSelector{}
			case "affinity", "affinity-quota", "affinity-never":
				rebind := SessionAffinityRebindOnAny
				if strategy == "affinity-quota" {
					rebind = SessionAffinityRebindOnQuota
				} else if strategy == "affinity-never" {
					rebind = SessionAffinityRebindOnNever
				}
				affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &SoonestResetSelector{}, CodexPreferHigherPriority: true, RebindOn: rebind})
				t.Cleanup(affinity.Stop)
				selector = affinity
			}
			manager := NewManager(nil, selector, nil)
			manager.RegisterExecutor(&soonestResetExecutor{})
			models := []string{"gpt-reserve-a", "gpt-reserve-b"}
			deadline := time.Now().Add(24 * time.Hour)
			business := &Auth{ID: "reserve-business", Provider: "codex", Status: StatusActive,
				Attributes: map[string]string{"priority": "100"}, Metadata: map[string]any{MetadataQuotaReservePercent: 20},
				Quota: QuotaState{ObservedAt: time.Now(), Signals: map[string]string{
					"X-Codex-Primary-Window-Minutes": "10080", "X-Codex-Primary-Used-Percent": "79.99",
					"X-Codex-Primary-Reset-At": strconv.FormatInt(deadline.Unix(), 10),
				}},
			}
			credits := &Auth{ID: "reserve-credits", Provider: "codex", Status: StatusActive, Quota: business.Quota.Clone()}
			credits.Quota.Signals["X-Codex-Primary-Used-Percent"] = "100"
			for _, auth := range []*Auth{business, credits} {
				if _, err := manager.Register(context.Background(), auth); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: models[0]}, {ID: models[1]}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			}
			pick := func(model, want string) {
				t.Helper()
				opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"reserve-session"}}}
				auth, _, err := manager.pickNext(context.Background(), "codex", model, opts, nil)
				if want == "" {
					if err == nil {
						t.Fatalf("pick(%s) = %v, want unavailable bound auth error", model, auth)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if auth == nil || auth.ID != want {
					t.Fatalf("pick(%s) = %v, want %s", model, auth, want)
				}
			}
			for _, model := range models {
				pick(model, business.ID)
			}
			ctx := internallogging.WithResponseHeadersHolder(context.Background())
			internallogging.SetResponseHeaders(ctx, http.Header{
				"X-Codex-Primary-Window-Minutes": []string{"10080"}, "X-Codex-Primary-Used-Percent": []string{"80"},
				"X-Codex-Primary-Reset-At": []string{strconv.FormatInt(deadline.Unix(), 10)},
			})
			manager.MarkResult(ctx, Result{AuthID: business.ID, Provider: "codex", Model: models[0], Success: true})
			// A result for model a must also protect the reserve when selecting b.
			for _, model := range []string{models[1], models[0]} {
				want := credits.ID
				if strategy == "affinity-never" {
					want = ""
				}
				pick(model, want)
			}
			updated, _ := manager.GetByID(business.ID)
			updated.Quota.Signals["X-Codex-Primary-Reset-At"] = strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)
			if _, err := manager.Update(context.Background(), updated); err != nil {
				t.Fatal(err)
			}
			for _, model := range models {
				pick(model, business.ID)
			}
		})
	}
}
