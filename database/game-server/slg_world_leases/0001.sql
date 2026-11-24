CREATE TABLE IF NOT EXISTS slg_world_leases (
    world_id VARBINARY(64) NOT NULL,
    writer VARBINARY(64) NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    PRIMARY KEY (world_id)
) ENGINE=InnoDB;
