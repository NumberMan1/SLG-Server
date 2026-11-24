CREATE TABLE IF NOT EXISTS slg_world_snapshot_checksums (
    world_id VARBINARY(64) NOT NULL,
    revision BIGINT UNSIGNED NOT NULL,
    digest BINARY(32) NOT NULL,
    PRIMARY KEY (world_id, revision)
) ENGINE=InnoDB;
