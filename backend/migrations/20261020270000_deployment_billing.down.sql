-- 回退顺序与 up 相反：先删子表（facts 持有 FK），再删父表（segments）。
-- IF EXISTS 让 `down` 在部分失败的库上也能收敛（platform-04 §11.2）。
DROP TABLE IF EXISTS deployment_cost_facts;
DROP TABLE IF EXISTS deployment_billing_segments;
