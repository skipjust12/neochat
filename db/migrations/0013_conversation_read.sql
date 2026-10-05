-- When the user last opened each chat. A chat whose latest reply is newer
-- (or that was never opened) is listed as unread. Chats that already
-- exist start out read.
ALTER TABLE conversation_metadata
    ADD COLUMN IF NOT EXISTS read_at TIMESTAMPTZ;

INSERT INTO conversation_metadata (user_id, conversation_id, read_at)
SELECT DISTINCT user_id, conversation_id, now()
FROM conversation_messages
ON CONFLICT (user_id, conversation_id) DO UPDATE SET read_at = now();
