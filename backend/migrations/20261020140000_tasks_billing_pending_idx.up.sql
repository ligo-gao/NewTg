-- ============================================================================
-- M6d Task 6b —— 终态补偿扫描的部分索引
--
-- 为什么需要：`ListReconcilable` 每轮 15s 扫描「终态 + 带 billing/settlement/
-- refund_pending 标记」的任务；没有索引时只能顺序扫全部历史终态行，任务量
-- 增长后会把 worker 的每轮固定成本拉成 O(历史总量)。
--
-- 为什么是部分索引：绝大多数终态任务已经封账（billing_final=true），
-- 谓词把它们排除在索引外，索引大小只跟随「待补偿」这一个很小的集合。
--
-- 为什么谓词里用 @>：private_data 是 jsonb，@> 是 immutable 且可走 GIN；
-- 这里用 btree(id) + 部分谓词，扫描天然按 id 升序，与 LIMIT 分页一致。
-- ============================================================================

CREATE INDEX IF NOT EXISTS tasks_billing_pending_idx
    ON tasks (id)
    WHERE state IN ('success', 'failure')
      AND (private_data @> '{"billing_pending": true}'::jsonb
           OR private_data @> '{"settlement_pending": true}'::jsonb
           OR private_data @> '{"refund_pending": true}'::jsonb);
