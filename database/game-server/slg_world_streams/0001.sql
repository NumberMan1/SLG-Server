CREATE TABLE IF NOT EXISTS slg_world_streams (
    world_id VARBINARY(64) NOT NULL,
    revision BIGINT UNSIGNED NOT NULL,
    PRIMARY KEY (world_id)
) ENGINE=InnoDB;
