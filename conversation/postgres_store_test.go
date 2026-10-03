//go:build integration

package conversation

import (
	"context"
	"testing"
	"time"

	"neochat/internal/dbtest"
)

func TestPostgresStore_AppendAndHistoryPreserveOrder(t *testing.T) {
	pgDB := dbtest.Postgres(t)
	dbtest.TruncateTables(t, pgDB, "conversation_messages", "conversation_summaries")
	store := NewPostgresStore(pgDB)
	ctx := context.Background()

	must(t, store.Append(ctx, "u1", "c1", Message{Role: RoleUser, Content: "hi", CreatedAt: time.Now().UTC()}))
	must(t, store.Append(ctx, "u1", "c1", Message{Role: RoleAssistant, Content: "hello", ModelID: "m1", CreatedAt: time.Now().UTC()}))

	history, err := store.History(ctx, "u1", "c1", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("len(History()) = %d, want 2", len(history))
	}
	if history[0].Role != RoleUser || history[0].Content != "hi" {
		t.Errorf("unexpected first message: %+v", history[0])
	}
	if history[1].Role != RoleAssistant || history[1].ModelID != "m1" {
		t.Errorf("unexpected second message: %+v", history[1])
	}
}

func TestPostgresStore_UnknownConversationReturnsEmptyNotError(t *testing.T) {
	pgDB := dbtest.Postgres(t)
	dbtest.TruncateTables(t, pgDB, "conversation_messages", "conversation_summaries")
	store := NewPostgresStore(pgDB)
	ctx := context.Background()

	history, err := store.History(ctx, "u1", "does-not-exist", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(history) != 0 {
		t.Errorf("len(History()) = %d, want 0", len(history))
	}
}

func TestPostgresStore_IsolatesByUserAndConversation(t *testing.T) {
	pgDB := dbtest.Postgres(t)
	dbtest.TruncateTables(t, pgDB, "conversation_messages", "conversation_summaries")
	store := NewPostgresStore(pgDB)
	ctx := context.Background()

	must(t, store.Append(ctx, "u1", "c1", Message{Role: RoleUser, Content: "u1/c1", CreatedAt: time.Now().UTC()}))
	must(t, store.Append(ctx, "u2", "c1", Message{Role: RoleUser, Content: "u2/c1", CreatedAt: time.Now().UTC()}))
	must(t, store.Append(ctx, "u1", "c2", Message{Role: RoleUser, Content: "u1/c2", CreatedAt: time.Now().UTC()}))

	h, err := store.History(ctx, "u1", "c1", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(h) != 1 || h[0].Content != "u1/c1" {
		t.Errorf("u1/c1 history = %+v, want exactly [u1/c1]", h)
	}
}

func TestPostgresStore_ProjectChatsStayOutOfGeneralList(t *testing.T) {
	pgDB := dbtest.Postgres(t)
	dbtest.TruncateTables(t, pgDB, "conversation_metadata", "conversation_messages", "projects")
	store := NewPostgresStore(pgDB)
	ctx := context.Background()
	now := time.Now().UTC()

	project := Project{ID: "project-1", Name: "Research", Description: "Use research context", CreatedAt: now}
	must(t, store.CreateProject(ctx, "u1", project))
	must(t, store.Append(ctx, "u1", "project-chat", Message{Role: RoleUser, Content: "Project question", CreatedAt: now}))
	must(t, store.UpdateMetadata(ctx, "u1", "project-chat", MetadataUpdate{ProjectID: &project.ID}))

	general, err := store.List(ctx, "u1", 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	projectChats, err := store.ListByProject(ctx, "u1", project.ID, 10)
	if err != nil {
		t.Fatalf("ListByProject: %v", err)
	}
	if len(general) != 0 || len(projectChats) != 1 || projectChats[0].ID != "project-chat" {
		t.Fatalf("general=%+v project=%+v", general, projectChats)
	}

	projects, err := store.ListProjects(ctx, "u1")
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 1 || projects[0].Description != project.Description {
		t.Fatalf("projects=%+v", projects)
	}
}

func TestPostgresStore_GetSummaryUnknownConversationReturnsZeroValue(t *testing.T) {
	pgDB := dbtest.Postgres(t)
	dbtest.TruncateTables(t, pgDB, "conversation_messages", "conversation_summaries")
	store := NewPostgresStore(pgDB)
	ctx := context.Background()

	s, err := store.GetSummary(ctx, "u1", "does-not-exist")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Text != "" || s.CoversThrough != 0 {
		t.Errorf("GetSummary() = %+v, want the zero Summary", s)
	}
}

func TestPostgresStore_SetSummaryThenGetSummaryRoundTrips(t *testing.T) {
	pgDB := dbtest.Postgres(t)
	dbtest.TruncateTables(t, pgDB, "conversation_messages", "conversation_summaries")
	store := NewPostgresStore(pgDB)
	ctx := context.Background()

	want := Summary{Text: "the user asked about X and Y", CoversThrough: 5, UpdatedAt: time.Now().UTC()}
	must(t, store.SetSummary(ctx, "u1", "c1", want))

	got, err := store.GetSummary(ctx, "u1", "c1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Text != want.Text || got.CoversThrough != want.CoversThrough {
		t.Errorf("GetSummary() = %+v, want %+v", got, want)
	}

	// Round 2: SetSummary must upsert, not fail on a second call for the
	// same (user_id, conversation_id).
	want2 := Summary{Text: "updated summary", CoversThrough: 9, UpdatedAt: time.Now().UTC()}
	must(t, store.SetSummary(ctx, "u1", "c1", want2))
	got2, err := store.GetSummary(ctx, "u1", "c1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got2.Text != want2.Text || got2.CoversThrough != want2.CoversThrough {
		t.Errorf("GetSummary() after update = %+v, want %+v", got2, want2)
	}
}

func TestPostgresStore_SummaryIsolatesByUserAndConversation(t *testing.T) {
	pgDB := dbtest.Postgres(t)
	dbtest.TruncateTables(t, pgDB, "conversation_messages", "conversation_summaries")
	store := NewPostgresStore(pgDB)
	ctx := context.Background()

	must(t, store.SetSummary(ctx, "u1", "c1", Summary{Text: "u1/c1 summary", UpdatedAt: time.Now().UTC()}))
	must(t, store.SetSummary(ctx, "u2", "c1", Summary{Text: "u2/c1 summary", UpdatedAt: time.Now().UTC()}))

	got, err := store.GetSummary(ctx, "u1", "c1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Text != "u1/c1 summary" {
		t.Errorf("GetSummary(u1, c1) = %+v, want Text=%q", got, "u1/c1 summary")
	}
}

func TestPostgresStore_AttachmentsRoundTrip(t *testing.T) {
	pgDB := dbtest.Postgres(t)
	dbtest.TruncateTables(t, pgDB, "conversation_messages", "conversation_summaries")
	store := NewPostgresStore(pgDB)
	ctx := context.Background()

	refs := []Attachment{{ID: "f1", Name: "a.png", MIME: "image/png", Kind: "image", Size: 3}}
	must(t, store.Append(ctx, "u1", "c1", Message{Role: RoleUser, Content: "", Attachments: refs, CreatedAt: time.Now().UTC()}))
	must(t, store.Append(ctx, "u1", "c1", Message{Role: RoleAssistant, Content: "nice cat", CreatedAt: time.Now().UTC()}))

	history, err := store.History(ctx, "u1", "c1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || len(history[0].Attachments) != 1 || history[0].Attachments[0] != refs[0] || history[1].Attachments != nil {
		t.Fatalf("History attachments = %+v", history)
	}
	page, err := store.HistoryPage(ctx, "u1", "c1", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 2 || len(page.Messages[0].Attachments) != 1 {
		t.Fatalf("HistoryPage attachments = %+v", page.Messages)
	}
}

func TestPostgresStore_DeleteRemovesTheChatsFiles(t *testing.T) {
	pgDB := dbtest.Postgres(t)
	dbtest.TruncateTables(t, pgDB, "conversation_messages", "conversation_summaries", "attachments")
	store := NewPostgresStore(pgDB)
	ctx := context.Background()

	must(t, store.Append(ctx, "u1", "c1", Message{Role: RoleUser, Content: "see file", CreatedAt: time.Now().UTC()}))
	for _, row := range [][2]string{{"f1", "c1"}, {"f2", "c2"}, {"f3", ""}} {
		if _, err := pgDB.ExecContext(ctx, `
			INSERT INTO attachments (user_id, attachment_id, conversation_id, name, mime, kind, size, data, created_at)
			VALUES ('u1', $1, $2, 'a.txt', 'text/plain', 'text', 1, 'x', now())`, row[0], row[1]); err != nil {
			t.Fatal(err)
		}
	}
	must(t, store.Delete(ctx, "u1", "c1"))

	var left []string
	rows, err := pgDB.QueryContext(ctx, `SELECT attachment_id FROM attachments ORDER BY attachment_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		must(t, rows.Scan(&id))
		left = append(left, id)
	}
	if len(left) != 2 || left[0] != "f2" || left[1] != "f3" {
		t.Fatalf("files left after deleting c1 = %v, want [f2 f3]", left)
	}
}

func TestPostgresStore_DeleteRemovesTheChatsPages(t *testing.T) {
	pgDB := dbtest.Postgres(t)
	dbtest.TruncateTables(t, pgDB, "conversation_messages", "conversation_summaries", "web_pages")
	store := NewPostgresStore(pgDB)
	ctx := context.Background()

	must(t, store.Append(ctx, "u1", "c1", Message{Role: RoleUser, Content: "read this", CreatedAt: time.Now().UTC()}))
	for _, row := range [][2]string{{"https://a.dev", "c1"}, {"https://b.dev", "c1"}, {"https://a.dev", "c2"}} {
		if _, err := pgDB.ExecContext(ctx, `
			INSERT INTO web_pages (user_id, conversation_id, url, final_url, content, content_bytes, fetched_at)
			VALUES ('u1', $2, $1, $1, 'text', 4, now())`, row[0], row[1]); err != nil {
			t.Fatal(err)
		}
	}
	must(t, store.Delete(ctx, "u1", "c1"))
	var left int
	must(t, pgDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM web_pages WHERE conversation_id = 'c1'`).Scan(&left))
	var other int
	must(t, pgDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM web_pages WHERE conversation_id = 'c2'`).Scan(&other))
	if left != 0 || other != 1 {
		t.Fatalf("pages left: deleted chat %d (want 0), other chat %d (want 1)", left, other)
	}
}

func TestPostgresStore_AddResponseVersion(t *testing.T) {
	pgDB := dbtest.Postgres(t)
	dbtest.TruncateTables(t, pgDB, "conversation_messages", "conversation_summaries")
	store := NewPostgresStore(pgDB)
	ctx := context.Background()

	must(t, store.Append(ctx, "u1", "c1", Message{Role: RoleUser, Content: "hi", CreatedAt: time.Now().UTC()}))
	must(t, store.Append(ctx, "u1", "c1", Message{Role: RoleAssistant, Content: "first", ModelID: "m1", CreatedAt: time.Now().UTC()}))
	history, err := store.History(ctx, "u1", "c1", 0)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := store.AddResponseVersion(ctx, "u1", "c1", history[1].ID, ResponseVersion{Content: "second", ModelID: "m2", CreatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatalf("AddResponseVersion: %v", err)
	}
	if updated.Content != "second" || len(updated.Versions) != 2 || updated.Versions[0].Content != "first" {
		t.Fatalf("updated message = %+v", updated)
	}
}

func TestPostgresStore_WebActivityRoundTrip(t *testing.T) {
	pgDB := dbtest.Postgres(t)
	dbtest.TruncateTables(t, pgDB, "conversation_messages", "conversation_summaries")
	store := NewPostgresStore(pgDB)
	ctx := context.Background()
	now := time.Now().UTC()

	version := ResponseVersion{
		Content: "answer", ModelID: "m1", CreatedAt: now,
		Activity: []ToolActivity{{Tool: "web_search", Query: "q", Results: []WebLink{{Title: "A", URL: "https://a.example", Snippet: "s"}}}},
		Sources:  []WebLink{{Title: "A", URL: "https://a.example"}},
	}
	must(t, store.Append(ctx, "u1", "c1", Message{Role: RoleUser, Content: "q", CreatedAt: now}))
	must(t, store.Append(ctx, "u1", "c1", Message{Role: RoleAssistant, Content: "answer", ModelID: "m1", CreatedAt: now, Versions: []ResponseVersion{version}}))
	history, err := store.History(ctx, "u1", "c1", 0)
	if err != nil {
		t.Fatal(err)
	}
	got := history[1].Versions
	if len(got) != 1 || len(got[0].Activity) != 1 || got[0].Activity[0].Results[0].URL != "https://a.example" || len(got[0].Sources) != 1 {
		t.Fatalf("stored versions = %+v", got)
	}
	updated, err := store.AddResponseVersion(ctx, "u1", "c1", history[1].ID, ResponseVersion{Content: "again", CreatedAt: now, Activity: []ToolActivity{{Tool: "web_fetch", URL: "https://b.example"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Versions) != 2 || updated.Versions[0].Activity[0].Query != "q" || updated.Versions[1].Activity[0].URL != "https://b.example" {
		t.Fatalf("versions after regenerate = %+v", updated.Versions)
	}
}
