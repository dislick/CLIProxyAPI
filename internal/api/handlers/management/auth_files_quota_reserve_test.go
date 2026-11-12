package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	fileauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestQuotaReservePatchPersistsValidatesAndClears(t *testing.T) {
	authDir := t.TempDir()
	path := filepath.Join(authDir, "business.json")
	store := fileauth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	manager := coreauth.NewManager(store, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: "business.json", FileName: "business.json", Provider: "codex",
		Attributes: map[string]string{"path": path}, Metadata: map[string]any{"type": "codex"},
		Quota: coreauth.QuotaState{ObservedAt: time.Now(), Signals: map[string]string{
			"X-Codex-Primary-Used-Percent": "85", "X-Codex-Primary-Reset-After-Seconds": "3600",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	patch := func(value string) int {
		t.Helper()
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/v8/management/credentials/fields", strings.NewReader(`{"name":"business.json","quota_reserve_percent":`+value+`}`))
		handler.PatchAuthFileFields(ctx)
		return rec.Code
	}
	if status := patch("20"); status != http.StatusOK {
		t.Fatalf("patch status = %d", status)
	}
	current, _ := manager.GetByID("business.json")
	if coreauth.IsAuthAvailableForModel(current, "", time.Now()) {
		t.Fatal("updated reserve did not affect runtime eligibility")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := ojson.GetBytesNoCopy(raw, coreauth.MetadataQuotaReservePercent).Float(); got != 20 {
		t.Fatalf("persisted reserve = %v", got)
	}
	// A restart must reload the setting from the existing auth file.
	loaded, err := store.List(context.Background())
	if err != nil || len(loaded) != 1 {
		t.Fatalf("reload = %d credentials, %v", len(loaded), err)
	}
	if reserve, err := coreauth.AuthQuotaReservePercent(loaded[0]); err != nil || reserve != 20 {
		t.Fatalf("reloaded reserve = %v, %v", reserve, err)
	}
	for _, invalid := range []string{"-1", "101", "true", `"invalid"`, `{"percent":20}`} {
		if status := patch(invalid); status != http.StatusBadRequest {
			t.Fatalf("invalid reserve %s status = %d", invalid, status)
		}
	}
	current, _ = manager.GetByID("business.json")
	if reserve, _ := coreauth.AuthQuotaReservePercent(current); reserve != 20 {
		t.Fatal("rejected patches changed the setting")
	}
	if status := patch("null"); status != http.StatusOK {
		t.Fatalf("clear status = %d", status)
	}
	current, _ = manager.GetByID("business.json")
	if !coreauth.IsAuthAvailableForModel(current, "", time.Now()) {
		t.Fatal("cleared reserve still blocks account")
	}
}
