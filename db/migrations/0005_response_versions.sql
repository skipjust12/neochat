ALTER TABLE conversation_messages
    ADD COLUMN IF NOT EXISTS versions JSONB NOT NULL DEFAULT '[]'::jsonb;

UPDATE conversation_messages
SET versions = jsonb_build_array(jsonb_build_object(
    'content', content,
    'model_id', model_id,
    'created_at', created_at
))
WHERE role = 'assistant' AND jsonb_array_length(versions) = 0;
