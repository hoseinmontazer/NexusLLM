-- Configure upstream_model_name for faster-whisper-server
-- 
-- This script sets the correct model name mapping for Whisper transcription endpoints.
-- faster-whisper-server expects specific model size names (tiny, base, small, medium, large-v3, etc.)
-- instead of generic names like "whisper".

-- Example 1: Update existing endpoint to use large-v3
UPDATE model_endpoints 
SET upstream_model_name = 'large-v3',
    updated_at = NOW()
WHERE model_id = (
    SELECT id FROM models 
    WHERE name = 'whisper' 
    AND enabled = TRUE
    LIMIT 1
);

-- Example 2: Check what's currently configured
SELECT 
    m.name AS model_name,
    m.display_name,
    m.backend_type,
    me.host,
    me.port,
    me.upstream_model_name,
    me.health_status,
    me.is_enabled
FROM model_endpoints me
JOIN models m ON m.id = me.model_id
WHERE m.backend_type = 'cpu_native'
  AND m.enabled = TRUE;

-- Example 3: Update multiple Whisper variants
-- If you have different models for different sizes:

-- UPDATE model_endpoints 
-- SET upstream_model_name = 'base', updated_at = NOW()
-- WHERE model_id = (SELECT id FROM models WHERE name = 'whisper-base' LIMIT 1);

-- UPDATE model_endpoints 
-- SET upstream_model_name = 'medium', updated_at = NOW()
-- WHERE model_id = (SELECT id FROM models WHERE name = 'whisper-medium' LIMIT 1);

-- UPDATE model_endpoints 
-- SET upstream_model_name = 'large-v3', updated_at = NOW()
-- WHERE model_id = (SELECT id FROM models WHERE name = 'whisper-large' LIMIT 1);

-- Example 4: Verify the configuration took effect
SELECT 
    m.name,
    me.upstream_model_name,
    me.updated_at
FROM model_endpoints me
JOIN models m ON m.id = me.model_id
WHERE m.name LIKE '%whisper%';

-- Available faster-whisper model sizes:
-- ----------------------------------------
-- tiny, tiny.en
-- base, base.en  
-- small, small.en
-- medium, medium.en
-- large-v1, large-v2, large-v3, large
-- distil-large-v2, distil-large-v3
-- distil-medium.en, distil-small.en
--
-- Choose based on your deployed faster-whisper-server model.
-- The gateway will automatically reload within 10 seconds.
