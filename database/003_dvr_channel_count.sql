USE videos;

ALTER TABLE dvr_devices
  ADD COLUMN IF NOT EXISTS channel_count INT NOT NULL DEFAULT 16;

UPDATE dvr_devices d
SET channel_count = GREATEST(
  1,
  COALESCE((SELECT MAX(c.dvr_channel) FROM cameras c WHERE c.dvr_device_id=d.id AND c.source_type='dvr'), 1)
)
WHERE channel_count = 16;
