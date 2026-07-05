//go:build e2e

// Integration test that runs the Go SDK (sdk/go) against a real
// openviking-server subprocess. Exposes path/shape mismatches between
// what the SDK calls and what the server's routers register.
//
// Run: make build && (cd sdk/go && go test -tags=e2e -race -v -timeout 120s)
package openviking

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	integrationAddr = "127.0.0.1:1940"
	integrationURL  = "http://" + integrationAddr
)

// TestSDK_ServerCompatibility starts the openviking-server with the
// local-memory config and exercises each SDK method that has a known
// or suspected path mismatch. Each subtest asserts the call does not
// 404 — the goal is to surface route gaps, not verify full response
// shape correctness.
func TestSDK_ServerCompatibility(t *testing.T) {
	bin := locateServerBinary(t)
	cfgPath := locateConfig(t)

	srvCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(srvCtx, bin, "--config", cfgPath, "--addr", integrationAddr)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer func() {
		cancel()
		_ = cmd.Wait()
	}()

	if !waitForHealth(t, integrationURL+"/healthz", 30*time.Second) {
		t.Fatal("server never became healthy")
	}

	client, err := NewClient(Config{
		BaseURL: integrationURL,
		Account: "e2e",
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.CloseIdleConnections()

	ctx := context.Background()

	// ---- Health (no mismatch expected) ----
	if ok, err := client.Health(ctx); err != nil || !ok {
		t.Errorf("Health: ok=%v err=%v", ok, err)
	}

	// ---- Resources / Content ----
	t.Run("AddResource", func(t *testing.T) {
		_, err := client.AddResource(ctx, "/tmp/nonexistent-sdk-test.txt", &AddResourceOptions{Reason: "e2e"})
		// We expect an error (file doesn't exist), but it should NOT be a 404 route error.
		if err != nil && isRoute404(err) {
			t.Errorf("AddResource hit route 404: %v", err)
		}
	})

	t.Run("Write_Read", func(t *testing.T) {
		// Write then Read back via SDK.
		_, err := client.Write(ctx, "/sdk_e2e.txt", "hello from sdk e2e", nil)
		if err != nil {
			t.Errorf("Write: %v", err)
			return
		}
		got, err := client.Read(ctx, "/sdk_e2e.txt", 0, 0)
		if err != nil {
			t.Errorf("Read: %v", err)
			return
		}
		if !strings.Contains(got, "hello from sdk e2e") {
			t.Errorf("Read: body mismatch; got %q", got)
		}
	})

	// ---- Search / Find / Grep (known mismatches) ----
	t.Run("Find", func(t *testing.T) {
		_, err := client.Find(ctx, "hello", nil)
		if err != nil && isRoute404(err) {
			t.Errorf("Find hit route 404 (server missing /api/v1/search/find): %v", err)
		}
	})

	t.Run("Search", func(t *testing.T) {
		_, err := client.Search(ctx, "hello", nil)
		if err != nil && isRoute404(err) {
			t.Errorf("Search hit route 404 (server missing /api/v1/search/search): %v", err)
		}
	})

	t.Run("Grep", func(t *testing.T) {
		// Write a file with known content, then grep for it.
		_, err := client.Write(ctx, "/sdk_e2e_grep.txt", "hello world from sdk e2e grep", nil)
		if err != nil {
			t.Errorf("Write (grep setup): %v", err)
			return
		}
		result, err := client.Grep(ctx, "/", "hello world", nil)
		if err != nil && isRoute404(err) {
			t.Errorf("Grep hit route 404 (server missing /api/v1/search/grep): %v", err)
			return
		}
		if err != nil {
			t.Errorf("Grep: %v", err)
			return
		}
		// Shape assertion: result must be a map with a "matches" field.
		matches, ok := result["matches"]
		if !ok {
			t.Errorf("Grep: response missing 'matches' field; got %v", result)
			return
		}
		// matches should be a non-empty slice (we wrote content containing the pattern).
		if s, ok := matches.([]any); !ok || len(s) == 0 {
			t.Errorf("Grep: expected non-empty matches slice, got %T %v", matches, matches)
		}
	})

	t.Run("Glob", func(t *testing.T) {
		// Write a file with a known name, then glob for it.
		_, err := client.Write(ctx, "/sdk_e2e_glob_target.txt", "glob content", nil)
		if err != nil {
			t.Errorf("Write (glob setup): %v", err)
			return
		}
		result, err := client.Glob(ctx, "sdk_e2e_glob_target.txt", "/")
		if err != nil && isRoute404(err) {
			t.Errorf("Glob hit route 404 (server missing /api/v1/search/glob): %v", err)
			return
		}
		if err != nil {
			t.Errorf("Glob: %v", err)
			return
		}
		// Shape assertion: result must be a map with a "matches" field.
		matches, ok := result["matches"]
		if !ok {
			t.Errorf("Glob: response missing 'matches' field; got %v", result)
			return
		}
		if s, ok := matches.([]any); !ok || len(s) == 0 {
			t.Errorf("Glob: expected non-empty matches slice, got %T %v", matches, matches)
		}
	})

	// ---- Sessions (known mismatches: /messages vs /turns, /context vs /memory) ----
	t.Run("Sessions_CreateListGet", func(t *testing.T) {
		created, err := client.CreateSession(ctx, &CreateSessionOptions{SessionID: "sdk-e2e-session"})
		if err != nil {
			t.Errorf("CreateSession: %v", err)
			return
		}
		// Shape assertion: created result should carry the caller-supplied session id.
		if createdID, _ := created["id"].(string); createdID != "sdk-e2e-session" {
			t.Errorf("CreateSession: result.id = %q, want %q", createdID, "sdk-e2e-session")
		}
		list, err := client.ListSessions(ctx)
		if err != nil {
			t.Errorf("ListSessions: %v", err)
			return
		}
		// Shape assertion: ListSessions returns an array; the created session must be in it.
		found := false
		for _, item := range list {
			if m, ok := item.(map[string]any); ok {
				if id, _ := m["id"].(string); id == "sdk-e2e-session" {
					found = true
					break
				}
			}
		}
		if !found {
			t.Errorf("ListSessions: created session sdk-e2e-session not in list %v", list)
		}
		got, err := client.GetSession(ctx, "sdk-e2e-session", nil)
		if err != nil {
			t.Errorf("GetSession: %v", err)
			return
		}
		if gotID, _ := got["id"].(string); gotID != "sdk-e2e-session" {
			t.Errorf("GetSession: result.id = %q, want %q", gotID, "sdk-e2e-session")
		}
	})

	t.Run("AddMessage", func(t *testing.T) {
		content := "hi from sdk e2e"
		result, err := client.AddMessage(ctx, "sdk-e2e-session", "user", AddMessageOptions{Content: &content})
		if err != nil && isRoute404(err) {
			t.Errorf("AddMessage hit route 404 (server missing /api/v1/sessions/:id/messages): %v", err)
			return
		}
		if err != nil {
			t.Errorf("AddMessage: %v", err)
			return
		}
		// Shape assertion: result must confirm the session id.
		if sid, _ := result["session_id"].(string); sid != "sdk-e2e-session" {
			t.Errorf("AddMessage: result.session_id = %q, want %q", sid, "sdk-e2e-session")
		}
	})

	t.Run("BatchAddMessages", func(t *testing.T) {
		c1 := "batch msg 1"
		c2 := "batch msg 2"
		result, err := client.BatchAddMessages(ctx, "sdk-e2e-session", []Message{
			{Role: "user", Content: &c1},
			{Role: "assistant", Content: &c2},
		}, nil)
		if err != nil && isRoute404(err) {
			t.Errorf("BatchAddMessages hit route 404 (server missing /api/v1/sessions/:id/messages/batch): %v", err)
			return
		}
		if err != nil {
			t.Errorf("BatchAddMessages: %v", err)
			return
		}
		// Shape assertion: result must report the count of appended messages.
		if appended, _ := result["appended"].(float64); appended != 2 {
			t.Errorf("BatchAddMessages: result.appended = %v, want 2", result["appended"])
		}
	})

	t.Run("GetSessionContext", func(t *testing.T) {
		_, err := client.GetSessionContext(ctx, "sdk-e2e-session", 1000)
		if err != nil && isRoute404(err) {
			t.Errorf("GetSessionContext hit route 404 (server missing /api/v1/sessions/:id/context): %v", err)
		}
	})

	t.Run("CommitSession", func(t *testing.T) {
		_, err := client.CommitSession(ctx, "sdk-e2e-session", nil)
		if err != nil && isRoute404(err) {
			t.Errorf("CommitSession hit route 404: %v", err)
		}
	})

	t.Run("DeleteSession", func(t *testing.T) {
		err := client.DeleteSession(ctx, "sdk-e2e-session")
		if err != nil && isRoute404(err) {
			t.Errorf("DeleteSession hit route 404: %v", err)
		}
	})

	// ---- Filesystem (known: /fs/attrs missing) ----
	t.Run("Stat", func(t *testing.T) {
		result, err := client.Stat(ctx, "/sdk_e2e.txt")
		if err != nil && isRoute404(err) {
			t.Errorf("Stat hit route 404: %v", err)
			return
		}
		if err != nil {
			t.Errorf("Stat: %v", err)
			return
		}
		// Shape assertion: result must carry the path.
		if p, _ := result["path"].(string); p == "" {
			t.Errorf("Stat: result.path empty; got %v", result)
		}
	})

	t.Run("Attrs", func(t *testing.T) {
		result, err := client.Attrs(ctx, "/sdk_e2e.txt")
		if err != nil && isRoute404(err) {
			t.Errorf("Attrs hit route 404 (server missing /api/v1/fs/attrs): %v", err)
			return
		}
		if err != nil {
			t.Errorf("Attrs: %v", err)
			return
		}
		// Shape assertion: result must carry the path.
		if p, _ := result["path"].(string); p == "" {
			t.Errorf("Attrs: result.path empty; got %v", result)
		}
	})

	t.Run("List", func(t *testing.T) {
		_, err := client.List(ctx, "/", nil)
		if err != nil && isRoute404(err) {
			t.Errorf("List hit route 404: %v", err)
		}
	})

	t.Run("Tree", func(t *testing.T) {
		_, err := client.Tree(ctx, "/", nil)
		if err != nil && isRoute404(err) {
			t.Errorf("Tree hit route 404: %v", err)
		}
	})

	// ---- Tasks ----
	t.Run("ListTasks", func(t *testing.T) {
		_, err := client.ListTasks(ctx, nil)
		if err != nil && isRoute404(err) {
			t.Errorf("ListTasks hit route 404: %v", err)
		}
	})

	// ---- Skills (known: /skills/find, /skills/validate) ----
	t.Run("ListSkills", func(t *testing.T) {
		_, err := client.ListSkills(ctx, nil)
		if err != nil && isRoute404(err) {
			t.Errorf("ListSkills hit route 404: %v", err)
		}
	})

	t.Run("FindSkills", func(t *testing.T) {
		result, err := client.FindSkills(ctx, "test", nil)
		if err != nil && isRoute404(err) {
			t.Errorf("FindSkills hit route 404 (server missing /api/v1/skills/find): %v", err)
			return
		}
		if err != nil {
			t.Errorf("FindSkills: %v", err)
			return
		}
		// Shape assertion: result must be a map with a "skills" field (array).
		if _, ok := result["skills"]; !ok {
			t.Errorf("FindSkills: response missing 'skills' field; got %v", result)
		}
	})

	// ---- Filesystem CRUD (Mkdir/Move/Remove/SetTags) ----
	t.Run("Mkdir_Move_Remove", func(t *testing.T) {
		if err := client.Mkdir(ctx, "/sdk_e2e_dir", "test dir"); err != nil {
			t.Errorf("Mkdir: %v", err)
			return
		}
		// Write a file inside the dir, then move the dir.
		if _, err := client.Write(ctx, "/sdk_e2e_dir/inner.txt", "inner content", nil); err != nil {
			t.Errorf("Write (mkdir setup): %v", err)
			return
		}
		if err := client.Move(ctx, "/sdk_e2e_dir", "/sdk_e2e_dir_moved"); err != nil {
			t.Errorf("Move: %v", err)
			return
		}
		// Verify the moved file exists at the new path.
		if _, err := client.Stat(ctx, "/sdk_e2e_dir_moved/inner.txt"); err != nil {
			t.Errorf("Stat (after move): %v", err)
		}
		// Remove the moved dir recursively.
		if err := client.Remove(ctx, "/sdk_e2e_dir_moved", &RemoveOptions{Recursive: true}); err != nil {
			t.Errorf("Remove: %v", err)
			return
		}
		// Verify the dir is gone.
		if _, err := client.Stat(ctx, "/sdk_e2e_dir_moved"); err == nil {
			t.Errorf("Stat (after remove): expected error, got nil")
		}
	})

	t.Run("SetTags", func(t *testing.T) {
		// Write a file, then set tags on it.
		if _, err := client.Write(ctx, "/sdk_e2e_tags.txt", "tagged content", nil); err != nil {
			t.Errorf("Write (set_tags setup): %v", err)
			return
		}
		result, err := client.SetTags(ctx, "/sdk_e2e_tags.txt", []string{"env:e2e", "type:test"}, nil)
		if err != nil && isRoute404(err) {
			t.Errorf("SetTags hit route 404 (server missing /api/v1/fs/attrs/set_tags): %v", err)
			return
		}
		if err != nil {
			t.Errorf("SetTags: %v", err)
		}
		_ = result
	})

	// ---- Content layers (Abstract/Overview/Reindex) ----
	t.Run("Abstract_Overview_Reindex", func(t *testing.T) {
		// Write a file with enough content to have an abstract.
		if _, err := client.Write(ctx, "/sdk_e2e_layers.txt", "Layer test content for abstract and overview.", nil); err != nil {
			t.Errorf("Write (layers setup): %v", err)
			return
		}
		// Abstract — server may 404 the sidecar layer when not yet indexed; only route-404 fails.
		if _, err := client.Abstract(ctx, "/sdk_e2e_layers.txt"); err != nil && isRoute404(err) {
			t.Errorf("Abstract hit route 404: %v", err)
		}
		// Overview — same tolerance.
		if _, err := client.Overview(ctx, "/sdk_e2e_layers.txt"); err != nil && isRoute404(err) {
			t.Errorf("Overview hit route 404: %v", err)
		}
		// Reindex — triggers reindexing; non-route-404 is acceptable.
		if _, err := client.Reindex(ctx, "/sdk_e2e_layers.txt", nil); err != nil && isRoute404(err) {
			t.Errorf("Reindex hit route 404: %v", err)
		}
	})

	// ---- Skills CRUD ----
	t.Run("Skills_CRUD", func(t *testing.T) {
		// Create a skill via AddSkill with inline data (map).
		skillData := map[string]any{
			"name":        "sdk-e2e-skill",
			"description": "skill created by sdk e2e test",
			"trigger":     "manual",
			"level":       0,
			"steps":       []map[string]any{{"action": "log", "params": map[string]any{"msg": "hi"}}},
		}
		createRes, err := client.AddSkill(ctx, skillData, nil)
		if err != nil && isRoute404(err) {
			t.Errorf("AddSkill hit route 404: %v", err)
			return
		}
		if err != nil {
			t.Errorf("AddSkill: %v", err)
			return
		}
		_ = createRes
		// GetSkill — verify the skill is retrievable.
		gotRes, err := client.GetSkill(ctx, "sdk-e2e-skill", nil)
		if err != nil && isRoute404(err) {
			t.Errorf("GetSkill hit route 404: %v", err)
			return
		}
		if err != nil {
			t.Errorf("GetSkill: %v", err)
			return
		}
		_ = gotRes
		// UpdateSkill — replace with a new description.
		updateData := map[string]any{
			"name":        "sdk-e2e-skill",
			"description": "updated by sdk e2e test",
			"trigger":     "manual",
			"level":       0,
			"steps":       []map[string]any{},
		}
		if _, err := client.UpdateSkill(ctx, "sdk-e2e-skill", updateData, nil); err != nil && isRoute404(err) {
			t.Errorf("UpdateSkill hit route 404: %v", err)
		}
		// ValidateSkill — validate a skill payload without installing.
		validateData := map[string]any{
			"name":        "sdk-e2e-validate",
			"description": "validation target",
			"trigger":     "manual",
			"level":       0,
			"steps":       []map[string]any{},
		}
		if _, err := client.ValidateSkill(ctx, validateData, nil); err != nil && isRoute404(err) {
			t.Errorf("ValidateSkill hit route 404: %v", err)
		}
		// DeleteSkill — remove the skill.
		if _, err := client.DeleteSkill(ctx, "sdk-e2e-skill"); err != nil && isRoute404(err) {
			t.Errorf("DeleteSkill hit route 404: %v", err)
		}
	})

	// ---- Tasks (GetTask on nonexistent ID — SDK returns nil, nil) ----
	t.Run("GetTask", func(t *testing.T) {
		got, err := client.GetTask(ctx, "nonexistent-task-id")
		if err != nil && isRoute404(err) {
			t.Errorf("GetTask hit route 404: %v", err)
			return
		}
		// SDK contract: GetTask on NOT_FOUND returns (nil, nil).
		if err != nil {
			t.Errorf("GetTask: expected nil error for NOT_FOUND, got %v", err)
		}
		if got != nil {
			t.Errorf("GetTask: expected nil result for NOT_FOUND, got %v", got)
		}
	})

	// ---- SessionExists (indirect: calls GetSession) ----
	t.Run("SessionExists", func(t *testing.T) {
		// Create a session, then check exists.
		if _, err := client.CreateSession(ctx, &CreateSessionOptions{SessionID: "sdk-e2e-exists"}); err != nil {
			t.Errorf("CreateSession (exists setup): %v", err)
			return
		}
		exists, err := client.SessionExists(ctx, "sdk-e2e-exists")
		if err != nil && isRoute404(err) {
			t.Errorf("SessionExists hit route 404: %v", err)
			return
		}
		if err != nil {
			t.Errorf("SessionExists: %v", err)
			return
		}
		if !exists {
			t.Errorf("SessionExists: expected true for existing session")
		}
		// Nonexistent session should return false.
		exists2, err := client.SessionExists(ctx, "nonexistent-session")
		if err != nil {
			t.Errorf("SessionExists (nonexistent): %v", err)
			return
		}
		if exists2 {
			t.Errorf("SessionExists: expected false for nonexistent session")
		}
		_ = client.DeleteSession(ctx, "sdk-e2e-exists")
	})

	// ---- GetSessionArchive ----
	t.Run("GetSessionArchive", func(t *testing.T) {
		// Create + archive a session, then fetch the archive.
		if _, err := client.CreateSession(ctx, &CreateSessionOptions{SessionID: "sdk-e2e-archive"}); err != nil {
			t.Errorf("CreateSession (archive setup): %v", err)
			return
		}
		if _, err := client.CommitSession(ctx, "sdk-e2e-archive", nil); err != nil {
			t.Errorf("CommitSession (archive setup): %v", err)
		}
		// GetSessionArchive with a stub archive_id — server returns the session.
		_, err := client.GetSessionArchive(ctx, "sdk-e2e-archive", "stub-archive-id")
		if err != nil && isRoute404(err) {
			t.Errorf("GetSessionArchive hit route 404: %v", err)
		}
		_ = client.DeleteSession(ctx, "sdk-e2e-archive")
	})

	// ---- System / Observer ----
	t.Run("WaitProcessed", func(t *testing.T) {
		result, err := client.WaitProcessed(ctx, nil)
		if err != nil && isRoute404(err) {
			t.Errorf("WaitProcessed hit route 404: %v", err)
			return
		}
		if err != nil {
			t.Errorf("WaitProcessed: %v", err)
			return
		}
		if _, ok := result["processed"]; !ok {
			t.Errorf("WaitProcessed: response missing 'processed' field; got %v", result)
		}
	})

	t.Run("CheckConsistency", func(t *testing.T) {
		// Write a file, then check consistency.
		if _, err := client.Write(ctx, "/sdk_e2e_consistency.txt", "consistency test", nil); err != nil {
			t.Errorf("Write (consistency setup): %v", err)
			return
		}
		result, err := client.CheckConsistency(ctx, "/sdk_e2e_consistency.txt")
		if err != nil && isRoute404(err) {
			t.Errorf("CheckConsistency hit route 404: %v", err)
			return
		}
		if err != nil {
			t.Errorf("CheckConsistency: %v", err)
			return
		}
		if _, ok := result["consistent"]; !ok {
			t.Errorf("CheckConsistency: response missing 'consistent' field; got %v", result)
		}
	})

	t.Run("GetStatus_IsHealthy", func(t *testing.T) {
		// GetStatus returns observer/system rollup.
		status, err := client.GetStatus(ctx)
		if err != nil && isRoute404(err) {
			t.Errorf("GetStatus hit route 404: %v", err)
			return
		}
		if err != nil {
			t.Errorf("GetStatus: %v", err)
			return
		}
		// IsHealthy reads status["is_healthy"].
		healthy, err := client.IsHealthy(ctx)
		if err != nil && isRoute404(err) {
			t.Errorf("IsHealthy hit route 404: %v", err)
			return
		}
		if err != nil {
			t.Errorf("IsHealthy: %v", err)
			return
		}
		_ = status
		_ = healthy
	})

	t.Run("QueueStatus", func(t *testing.T) {
		_, err := client.QueueStatus(ctx)
		if err != nil && isRoute404(err) {
			t.Errorf("QueueStatus hit route 404: %v", err)
		}
	})

	t.Run("VikingDBStatus", func(t *testing.T) {
		_, err := client.VikingDBStatus(ctx)
		if err != nil && isRoute404(err) {
			t.Errorf("VikingDBStatus hit route 404: %v", err)
		}
	})

	t.Run("ModelsStatus", func(t *testing.T) {
		_, err := client.ModelsStatus(ctx)
		if err != nil && isRoute404(err) {
			t.Errorf("ModelsStatus hit route 404: %v", err)
		}
	})

	// ---- Watches CRUD ----
	t.Run("Watches_CRUD", func(t *testing.T) {
		// Write a file to watch.
		if _, err := client.Write(ctx, "/sdk_e2e_watch.txt", "watch content", nil); err != nil {
			t.Errorf("Write (watch setup): %v", err)
			return
		}
		// ListWatches — should not 404.
		listRes, err := client.ListWatches(ctx, nil)
		if err != nil && isRoute404(err) {
			t.Errorf("ListWatches hit route 404: %v", err)
			return
		}
		_ = listRes
		// GetWatch on nonexistent ID — should not 404 (may 404 the resource,
		// but that's not a route 404).
		_, err = client.GetWatch(ctx, "nonexistent-watch-id", "")
		if err != nil && isRoute404(err) {
			t.Errorf("GetWatch hit route 404: %v", err)
		}
		// TriggerWatch on nonexistent ID — should not 404 the route.
		_, err = client.TriggerWatch(ctx, WatchRef{TaskID: "nonexistent-watch-id"})
		if err != nil && isRoute404(err) {
			t.Errorf("TriggerWatch hit route 404: %v", err)
		}
		// UpdateWatch on nonexistent ID — should not 404 the route.
		isActive := false
		_, err = client.UpdateWatch(ctx, UpdateWatchOptions{
			TaskID:   "nonexistent-watch-id",
			IsActive: &isActive,
		})
		if err != nil && isRoute404(err) {
			t.Errorf("UpdateWatch hit route 404: %v", err)
		}
		// DeleteWatch on nonexistent ID — should not 404 the route.
		_, err = client.DeleteWatch(ctx, WatchRef{TaskID: "nonexistent-watch-id"})
		if err != nil && isRoute404(err) {
			t.Errorf("DeleteWatch hit route 404: %v", err)
		}
	})

	// ---- Admin ----
	t.Run("Admin_Accounts_Users", func(t *testing.T) {
		// AdminListAccounts — should not 404.
		accounts, err := client.AdminListAccounts(ctx)
		if err != nil && isRoute404(err) {
			t.Errorf("AdminListAccounts hit route 404: %v", err)
			return
		}
		_ = accounts
		// AdminListUsers for the e2e account.
		users, err := client.AdminListUsers(ctx, "e2e")
		if err != nil && isRoute404(err) {
			t.Errorf("AdminListUsers hit route 404: %v", err)
			return
		}
		_ = users
		// AdminMigrate — should not 404.
		_, err = client.AdminMigrate(ctx, nil)
		if err != nil && isRoute404(err) {
			t.Errorf("AdminMigrate hit route 404: %v", err)
		}
	})

	// ---- Pack (Backup/Restore stubs) ----
	t.Run("BackupOVPack", func(t *testing.T) {
		// Write a file to back up.
		if _, err := client.Write(ctx, "/sdk_e2e_backup.txt", "backup content", nil); err != nil {
			t.Errorf("Write (backup setup): %v", err)
			return
		}
		// BackupOVPack downloads a tar to a local file.
		outPath, err := client.BackupOVPack(ctx, "", nil)
		if err != nil && isRoute404(err) {
			t.Errorf("BackupOVPack hit route 404: %v", err)
			return
		}
		_ = outPath
	})

	t.Run("RestoreOVPack", func(t *testing.T) {
		// RestoreOVPack uploads a file then posts temp_file_id. Without a
		// real temp upload endpoint this will fail, but it should not 404
		// the restore route. We expect a non-route-404 error.
		_, err := client.RestoreOVPack(ctx, "/nonexistent-pack.tar", nil)
		if err != nil && isRoute404(err) {
			t.Errorf("RestoreOVPack hit route 404: %v", err)
		}
	})
}

// isRoute404 reports whether err is an HTTP 404 from the server's
// router (i.e. the route is not registered). The server's NoRoute handler
// returns {error:{code:"RESOURCE_NOT_FOUND", message:"no route for <path>"}}
// so we match the "no route" signature to distinguish route 404s from
// resource 404s (which return "resource not found" with the same code).
func isRoute404(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "no route")
}

// locateServerBinary resolves ../../bin/openviking-server from the
// test file's source location via runtime.Caller (immune to os.Chdir).
func locateServerBinary(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// file = <root>/sdk/go/integration_test.go
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	bin := filepath.Join(root, "bin", "openviking-server")
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("binary %s not built (run `make build` from repo root): %v", bin, err)
	}
	return bin
}

// locateConfig resolves ../../examples/ov.conf.local-memory from the
// test file's source location.
func locateConfig(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	cfg := filepath.Join(root, "examples", "ov.conf.local-memory")
	if _, err := os.Stat(cfg); err != nil {
		t.Fatalf("config missing: %v", err)
	}
	return cfg
}

// waitForHealth polls /healthz until 200 or timeout.
func waitForHealth(t *testing.T, url string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil && resp.StatusCode == http.StatusOK {
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			return true
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// init chdir's to a temp dir so the server's relative ./data/ paths
// don't collide with the repo. Binary/config paths are resolved from
// the test file's source location, so this chdir is safe.
func init() {
	tmp, err := os.MkdirTemp("", "ov-sdk-e2e-")
	if err != nil {
		panic(fmt.Sprintf("mktemp: %v", err))
	}
	_ = os.Chdir(tmp)
	fmt.Println("sdk-e2e: working dir", tmp)
}
