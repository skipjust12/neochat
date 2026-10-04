-- The part of an earlier answer a user message asks about: selected in
-- the UI and sent with "Ask" (server/quote.go). Empty for most messages.
ALTER TABLE conversation_messages
    ADD COLUMN IF NOT EXISTS quote TEXT NOT NULL DEFAULT '';
