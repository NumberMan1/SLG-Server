CREATE TABLE IF NOT EXISTS slg_world_previous_snapshots (
    world_id VARBINARY(64) NOT NULL,
    revision BIGINT UNSIGNED NOT NULL,
    payload LONGBLOB NOT NULL,
    PRIMARY KEY (world_id)
) ENGINE=InnoDB;
