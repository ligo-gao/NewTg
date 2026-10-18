-- ============================================================================
-- platform-01 §6（channels）/ §10（orders）业务约束补齐（R-24）
--
-- 背景：初始化迁移 20261018053301 已落枚举、数值区间与财务字段约束，但 §6/§10
-- 字段表中「文档有、DDL 无」的几条规则在 M0 未进 init。init 已发布且必须保持
-- 不可变，故以新增迁移 + ALTER TABLE ADD CONSTRAINT 补齐；当前库数据为空，
-- 约束创建时即可全量校验通过（VALID）。
-- ============================================================================

-- ---- channels（platform-01 §6）----

-- §6：name 是渠道的全局唯一标识（1–64 字符、首尾空白裁剪）。varchar(64) 已兜住
-- 上限；这里补下限——裁剪后为空的名字（"" / 纯空白）虽能过 UNIQUE，却无法被
-- 管理员按名定位，故必须在 DB 层拒绝。btrim 的单参数形式只删 ASCII 空格，
-- 显式给出空白集合才能让「仅 tab/换行」的名字同样被拒。
ALTER TABLE channels ADD CONSTRAINT channels_name_len_chk
    CHECK (char_length(btrim(name, E' \t\r\n')) BETWEEN 1 AND 64);

-- §6：test_model 是连通性测试实际下发的模型，只有 ∈ supported_models 时才有
-- 意义；否则测试会向渠道发送它声明不支持的模型，得到误导性的「渠道不可用」。
-- jsonb 的 ? 运算符判断数组是否存在该字符串元素（test_model 为 NULL 时不检查）。
ALTER TABLE channels ADD CONSTRAINT channels_test_model_in_models_chk
    CHECK (test_model IS NULL OR supported_models ? test_model);

-- §6 tag：管理员自由标签（单值），是按标签批量启停/编辑的分组键。规则为
-- 1–32 字符；仅允许字母数字、._-: 与中日韩文字；禁止首尾空白。白名单本身已
-- 排除一切空白（含首尾），btrim 一项使「禁首尾空白」在约束文本中显式可见，
-- 便于按 spec 逐条对读。字符集的可移植性说明：[[:alnum:]] 在 C locale 下只匹配
-- ASCII 字母数字（UTF-8 locale 下才覆盖 Unicode）；因此中日韩文字由显式区段
-- （U+4E00–9FFF / U+3040–30FF / U+AC00–D7AF）保证，不依赖数据库 locale。
ALTER TABLE channels ADD CONSTRAINT channels_tag_chk
    CHECK (tag IS NULL OR (
        char_length(tag) BETWEEN 1 AND 32
        AND tag = btrim(tag)
        AND tag ~ '^[[:alnum:]._:一-鿿぀-ヿ가-힯-]+$'
    ));

-- ---- orders（platform-01 §10）----

-- §10：currency 是站点计价币种，只允许 ISO-4217 三字母大写代码；小写/空串/
-- 四位串会让对账与展示（金额最小单位换算）失去共同口径。char(3) 的补空格值
-- （如 'US '）同样被正则拒绝。
ALTER TABLE orders ADD CONSTRAINT orders_currency_chk
    CHECK (currency ~ '^[A-Z]{3}$');

-- §10：provider_currency 是渠道结算币种（与站点 currency 分离），可空；非空时
-- 同样必须是 ISO-4217 三字母大写代码。允许 NULL 表示该订单未记录渠道币种。
ALTER TABLE orders ADD CONSTRAINT orders_provider_currency_chk
    CHECK (provider_currency IS NULL OR provider_currency ~ '^[A-Z]{3}$');

-- §10：fx_rate_num / fx_rate_den 是「渠道币种 → CNY」汇率的分子/分母，必须成对
-- 出现；只有一半时换算无法计算，且会让对账口径（credited_credits 快照的来源）
-- 出现不可解释的空洞。两者各自 > 0 已由 init 的 orders_fx_rate_*_chk 保证。
ALTER TABLE orders ADD CONSTRAINT orders_fx_rate_pair_chk
    CHECK ((fx_rate_num IS NULL) = (fx_rate_den IS NULL));
