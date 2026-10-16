-- 仅用于真实 PG 集成测试。临时表在连接关闭时自动清除，不创建业务结构。
CREATE TEMP TABLE framework_tx_fixture (
    id BIGINT PRIMARY KEY,
    value TEXT NOT NULL UNIQUE
) ON COMMIT PRESERVE ROWS;
