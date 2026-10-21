-- 回滚订阅事件流（S1）。事件表没有外键依赖，可独立删除；
-- subscriptions 行本身不受影响（历史状态仍在，只是缺少事件解释）。
DROP TABLE IF EXISTS subscription_events;
