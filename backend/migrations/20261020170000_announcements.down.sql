-- 回退 D7a 公告两表。先删 reads（逻辑上从属于公告），再删主表；
-- 两表都没有 FK，删除顺序只表达依赖方向，不影响约束求值。
DROP TABLE IF EXISTS announcement_reads;
DROP TABLE IF EXISTS announcements;
