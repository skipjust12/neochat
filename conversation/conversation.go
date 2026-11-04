// Package conversation stores per-conversation chat history so a /chat
// request can send a model more than just the single latest message --
// see README's "Conversation storage schema built for migration from day
// one" pre-launch checklist item.
package conversation

import "time"

// Role identifies who sent a stored message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one stored turn in a conversation. Deliberately just these
// fields for now -- the checklist item asks for a schema that can grow
// (e.g. separating reasoning blocks from the final answer, or
// per-message metadata like "thought for N seconds") without breaking
// changes. A Go struct with JSON tags already satisfies that: new
// optional fields can be added later without touching what's here, as
// long as they default sanely (zero value) for older stored rows.
type Message struct {
	ID        int64  `json:"id,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Role      Role   `json:"role"`
	Content   string `json:"content"`

	// ModelID is which catalog model produced this message -- empty for
	// user turns, the router's selected model ID for assistant turns.
	// This is exactly the "which model answered a given message" example
	// the checklist item calls out as needing to be there from day one.
	ModelID   string    `json:"model_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`

	// IsSummary marks a message as machine-generated summary content
	// rather than a real turn someone typed/received -- not read anywhere
	// yet, reserved for when summarizer-produced text needs to be told
	// apart from ordinary history (e.g. excluding it from re-summarization
	// input, or flagging it in a transcript export).
	IsSummary bool              `json:"is_summary,omitempty"`
	Versions  []ResponseVersion `json:"versions,omitempty"`

	// Attachments lists the files sent with a user message. Only metadata
	// lives here; the bytes are in the attachment store, keyed by ID.
	Attachments []Attachment `json:"attachments,omitempty"`

	// Quote is the part of an earlier answer a user message asks about
	// (selected in the UI and sent with "Ask"); empty for most messages.
	Quote string `json:"quote,omitempty"`
}

// Attachment is the stored reference to one file sent with a message.
type Attachment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	MIME string `json:"mime"`
	Kind string `json:"kind"` // "image" | "document" | "text" | "video"
	Size int64  `json:"size"`
	// Width and Height are a generated picture's or video's size
	// (ResponseVersion.Images, Videos), so the client can hold its space
	// before it loads; Seconds is a generated video's length.
	Width   int     `json:"width,omitempty"`
	Height  int     `json:"height,omitempty"`
	Seconds float64 `json:"seconds,omitempty"`
}

const MaxRegenerationAttempts = 10

type ResponseVersion struct {
	Content   string    `json:"content"`
	ModelID   string    `json:"model_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`

	// Activity is what the model did on the web while writing this
	// version (searches with their results, pages it opened); Sources are
	// the pages it cited. Both live inside the versions JSON column.
	Activity []ToolActivity `json:"activity,omitempty"`
	Sources  []WebLink      `json:"sources,omitempty"`
	// ThoughtMS is how long the answer took before its text started (the
	// last time, if web steps came in between): "Thought for N seconds".
	ThoughtMS int64 `json:"thought_ms,omitempty"`
	// Questionnaire is set when the model ended this answer by asking the
	// user questions (the ask_user tool); the answers come back as the
	// user's next message.
	Questionnaire *Questionnaire `json:"questionnaire,omitempty"`

	// Images are the pictures an image model made for this answer, and
	// Videos the clip a video model made, kept with the chat's files
	// (server/image.go, server/video.go).
	Images []Attachment `json:"images,omitempty"`
	Videos []Attachment `json:"videos,omitempty"`
	// Files are what the model made on the server's terminal and sent to
	// the user with the answer (send_file), kept with the chat's files.
	Files []Attachment `json:"files,omitempty"`

	// Reasoning is the model's thinking before and while it wrote this
	// version, for models that share it; the client shows it on request.
	Reasoning string `json:"reasoning,omitempty"`
}

// Questionnaire is a set of questions the model put to the user, shown as
// a panel by the client: a title, the questions, and a closing note.
type Questionnaire struct {
	Title     string     `json:"title"`
	Footer    string     `json:"footer,omitempty"`
	Questions []Question `json:"questions"`
}

// Question is one question of a questionnaire. Besides its options the
// user can always write an answer of their own (the client's last row);
// a question without options takes only that.
type Question struct {
	ID          string           `json:"id"`
	Text        string           `json:"text"`
	Description string           `json:"description,omitempty"`
	MultiSelect bool             `json:"multi_select,omitempty"`
	Options     []QuestionOption `json:"options,omitempty"`
}

// QuestionOption is one answer to pick.
type QuestionOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// ToolActivity is one tool step: a search ("web_search", with the query
// and what came back), a page the model read ("web_fetch", with its title,
// the start of its text and its length), or something it ran on the
// server's terminal ("run_command", with the command; "run_script", with
// the script's language, purpose and code), with what came out.
type ToolActivity struct {
	Tool    string    `json:"tool"`
	Query   string    `json:"query,omitempty"`
	URL     string    `json:"url,omitempty"`
	Title   string    `json:"title,omitempty"`
	Results []WebLink `json:"results,omitempty"`
	Excerpt string    `json:"excerpt,omitempty"`
	Chars   int       `json:"chars,omitempty"`
	Error   string    `json:"error,omitempty"`

	// Terminal steps. Command is the command or the script's code,
	// Language "python" or "bash" (scripts), Purpose what the script is
	// for. ExitCode is set once it ran (-1: killed or stopped), Output is
	// the start and end of what it printed, OutputChars the whole length.
	Command     string `json:"command,omitempty"`
	Language    string `json:"language,omitempty"`
	Purpose     string `json:"purpose,omitempty"`
	ExitCode    *int   `json:"exit_code,omitempty"`
	Output      string `json:"output,omitempty"`
	OutputChars int    `json:"output_chars,omitempty"`
	TimedOut    bool   `json:"timed_out,omitempty"`
	DurationMS  int64  `json:"duration_ms,omitempty"`
	// Size is a sent file's size in bytes ("send_file", with its name as
	// Title and its path as Command).
	Size int64 `json:"size,omitempty"`

	// Pending marks a step still running; only ever set in the live
	// stream, never stored. Phase says what a pending terminal step is
	// doing: "writing" (the model is still writing the script) or
	// "running".
	Pending bool   `json:"pending,omitempty"`
	Phase   string `json:"phase,omitempty"`
}

// WebLink is a search hit or a cited page.
type WebLink struct {
	Title   string `json:"title,omitempty"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
}

// Overview is the compact representation used by conversation lists.
// The title is derived from the first user message, so saving a chat does
// not require a second metadata write or a separate naming flow.
type Overview struct {
	ID        string    `json:"conversation_id"`
	Title     string    `json:"title"`
	Pinned    bool      `json:"pinned,omitempty"`
	ProjectID string    `json:"project_id,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	// Unread: the chat has a reply newer than the last time the user
	// opened it (MarkRead).
	Unread bool `json:"unread,omitempty"`
}

type MetadataUpdate struct {
	Title     *string
	Pinned    *bool
	ProjectID *string
}

type Project struct {
	ID          string    `json:"project_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

// Summary is the rolling, machine-generated compression of a
// conversation's older messages, kept alongside the full History so a
// long-running conversation can stay within a model's context window
// without deleting anything from storage. See Store.GetSummary/SetSummary.
type Summary struct {
	// Text is the current summary content, folding in every message up to
	// CoversThrough.
	Text string `json:"text"`

	// CoversThrough is how many of the oldest messages in History (by
	// index, 0 meaning none) are already represented in Text. The next
	// summarization pass only needs to fold in History[CoversThrough:],
	// not the whole conversation again.
	CoversThrough int `json:"covers_through"`

	UpdatedAt time.Time `json:"updated_at"`
}

// Page is chronological, with a cursor for loading older messages.
type Page struct {
	Messages   []Message `json:"messages"`
	NextCursor int64     `json:"next_cursor,omitempty"`
}
