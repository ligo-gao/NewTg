-- 回退任务数据面主表（M6d Task 6a）。
-- 索引与约束随表删除级联移除；显式 DROP TABLE 是唯一需要的语句，
-- 拆成逐索引删除只会增加失败点而不改变结果。
DROP TABLE IF EXISTS tasks;
