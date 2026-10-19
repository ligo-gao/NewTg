-- 回退 M6c Task 4 的通知模板与投递记录。两表之间没有 FK，删除顺序不承载语义；
-- 先删投递记录（数据量更大、且其 template_key 逻辑引用模板 key）。
DROP TABLE IF EXISTS notification_deliveries;
DROP TABLE IF EXISTS notification_templates;
