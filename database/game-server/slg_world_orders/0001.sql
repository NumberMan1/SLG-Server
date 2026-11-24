CREATE TABLE IF NOT EXISTS slg_world_orders (
    world_id VARBINARY(64) NOT NULL,
    seat_revision BIGINT UNSIGNED NOT NULL,
    command_id VARBINARY(64) NOT NULL,
    target_tile_id INT UNSIGNED NOT NULL,
    troops INT UNSIGNED NOT NULL,
    combat_mode TINYINT UNSIGNED NOT NULL,
    reinforcement BIGINT UNSIGNED NOT NULL,
    retreat BIGINT UNSIGNED NOT NULL,
    PRIMARY KEY (world_id, seat_revision, command_id)
) ENGINE=InnoDB;
