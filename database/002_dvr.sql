USE videos;

CREATE TABLE IF NOT EXISTS dvr_devices (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  section_id BIGINT UNSIGNED NULL,
  name VARCHAR(150) NOT NULL,
  ip VARCHAR(64) NOT NULL,
  username VARCHAR(150) NULL,
  password TEXT NULL,
  enabled TINYINT(1) NOT NULL DEFAULT 1,
  sort_order INT NOT NULL DEFAULT 0,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uq_dvr_devices_ip (ip),
  KEY idx_dvr_devices_section (section_id),
  CONSTRAINT fk_dvr_devices_section FOREIGN KEY (section_id) REFERENCES sections(id) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

ALTER TABLE cameras
  ADD COLUMN IF NOT EXISTS source_type VARCHAR(20) NOT NULL DEFAULT 'ip',
  ADD COLUMN IF NOT EXISTS dvr_device_id BIGINT UNSIGNED NULL,
  ADD COLUMN IF NOT EXISTS dvr_channel INT NULL,
  ADD COLUMN IF NOT EXISTS rtsp_sub_url TEXT NULL;

CREATE INDEX IF NOT EXISTS idx_cameras_dvr_device ON cameras (dvr_device_id);
CREATE INDEX IF NOT EXISTS idx_cameras_source_type ON cameras (source_type);

ALTER TABLE cameras
  ADD CONSTRAINT fk_cameras_dvr_device FOREIGN KEY (dvr_device_id) REFERENCES dvr_devices(id) ON DELETE CASCADE;
