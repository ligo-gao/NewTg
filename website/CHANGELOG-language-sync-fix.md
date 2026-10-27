# 语言切换跨应用同步修复

## 问题描述

用户在营销网站（website）选择英文后，点击"控制台"跳转到控制台应用（frontend），语言又变回中文。

## Root Cause Analysis（根本原因分析）

使用 systematic-debugging 技能进行了系统性追踪，发现根本原因：

### 数据流追踪

```
用户操作：营销网站首页 → 选择 English
         ↓
营销网站：localStorage['newtg-locale'] = 'en'  ✓ 保存成功
         ↓
用户操作：点击"控制台"链接
         ↓
页面跳转：/en/ → /dashboard
         ↓
控制台应用启动：读取 localStorage['newtg.language']  ✗ 找不到
         ↓
回退逻辑：使用默认语言 'zh-CN'
         ↓
结果：用户看到中文界面 ❌
```

### 关键发现

两个应用使用**不同的 localStorage 键名**：

| 应用 | localStorage 键名 | 文件位置 |
|------|------------------|---------|
| 营销网站 (website) | `newtg-locale` | `website/src/components/language-switcher.tsx:28` |
| 控制台应用 (frontend) | `newtg.language` | `frontend/src/app/providers/i18n.tsx:160` |

由于键名不同，两个应用无法共享语言状态，导致语言选择在跨应用跳转时丢失。

## 解决方案

### 修复策略

**统一 localStorage 键名为 `newtg.language`**

选择控制台应用的键名作为标准，因为：
1. 控制台应用是主应用，已有完善的语言管理系统
2. 控制台应用的键名更符合命名规范（使用点号分隔命名空间）
3. 避免修改控制台应用的复杂 i18n 系统

### 实施的修改

#### 1. 更新营销网站的语言切换器

**文件：** `website/src/components/language-switcher.tsx`

```diff
  function handleChange(event: ChangeEvent<HTMLSelectElement>) {
    const next = event.target.value as Locale;
    try {
-     localStorage.setItem("newtg-locale", next);
+     // 使用与控制台应用相同的键名，确保跨应用共享语言状态
+     localStorage.setItem("newtg.language", next);
    } catch {
      // Storage can be unavailable; navigation still works.
    }
```

#### 2. 更新测试用例

**文件：** `website/src/components/language-switcher.test.tsx`

```diff
- expect(localStorage.getItem("newtg-locale")).toBe("en");
+ expect(localStorage.getItem("newtg.language")).toBe("en");
```

所有 10 个测试用例的断言都已更新。

## 验证结果

### 单元测试 ✅

```
 Test Files  1 passed (1)
      Tests  10 passed (10)
```

所有测试通过，包括：
- 中文 ↔ 英文切换
- 英文 ↔ 法语切换
- 定价页 ↔ 首页切换
- localStorage 不可用时的降级处理

### 构建验证 ✅

```
✓ Compiled successfully
✓ TypeScript type check passed
✓ Generated 16 static pages
```

前端成功重新构建，所有静态页面生成完成。

## 工作原理

### 修复后的数据流

```
用户操作：营销网站首页 → 选择 English
         ↓
营销网站：localStorage['newtg.language'] = 'en'  ✓ 保存
         ↓
用户操作：点击"控制台"链接
         ↓
页面跳转：/en/ → /dashboard
         ↓
控制台应用启动：读取 localStorage['newtg.language']  ✓ 找到 'en'
         ↓
应用语言：使用 'en'
         ↓
结果：用户看到英文界面 ✅
```

### 语言优先级（控制台应用）

根据 `frontend/src/app/providers/i18n.tsx` 的实现，控制台应用的语言决策优先级为：

1. **服务端偏好**（已登录用户）：`GET /api/self` 返回的用户语言设置
2. **本地 localStorage**：`localStorage['newtg.language']`（现在与营销网站共享）
3. **产品默认语言**：`zh-CN`（简体中文）

修复后，营销网站的语言选择会写入第 2 优先级，控制台应用能正确读取并应用。

## 测试场景

### 场景 1：未登录用户选择语言

1. 访问 https://10.211.55.5:8443/
2. 选择 "English" → 跳转到 `/en/`
3. 点击导航栏的 "控制台" → 跳转到 `/dashboard`
4. **预期结果**：控制台界面显示英文 ✅

### 场景 2：切换语言后访问定价页

1. 访问营销网站首页，选择 "Français"
2. 点击 "Pricing" → 跳转到 `/fr/pricing/`
3. 点击 "控制台" → 跳转到 `/dashboard`
4. **预期结果**：控制台界面显示法语 ✅

### 场景 3：已登录用户的语言优先级

1. 用户在控制台设置了用户偏好语言为 "日本語"
2. 用户访问营销网站，选择了 "English"
3. 用户点击"控制台"
4. **预期结果**：控制台显示日本語（服务端偏好优先级高于 localStorage）✅

### 场景 4：localStorage 不可用

1. 浏览器隐私模式或禁用了存储
2. 用户在营销网站选择语言
3. **预期结果**：当前页面语言正确，但跳转到控制台后回退到默认语言（降级行为）✅

## 兼容性注意事项

### 旧版本数据迁移

用户可能在 localStorage 中已经保存了旧键名 `newtg-locale` 的值。这不会造成问题：

- 旧数据会被忽略（控制台应用从未读取过 `newtg-locale`）
- 用户下次在营销网站选择语言时，会写入新键名 `newtg.language`
- 用户在控制台选择语言时，控制台应用会写入 `newtg.language`

**无需额外的迁移脚本**，因为这是一次性的小数据，用户的下一次操作就会自然覆盖。

### 后向兼容

- ✅ 营销网站：只写入新键名，不读取旧键名（营销网站通过 URL 路径确定当前语言）
- ✅ 控制台应用：无需修改，已经在使用 `newtg.language`

## 相关文件

### 修改的文件

- `website/src/components/language-switcher.tsx` - 语言切换器组件
- `website/src/components/language-switcher.test.tsx` - 单元测试

### 参考文件

- `frontend/src/app/providers/i18n.tsx` - 控制台应用的 i18n 提供者
- `website/src/config/locales.ts` - 营销网站的语言配置
- `CHANGELOG-typography.md` - 排版系统统一的变更日志

## 部署状态

- ✅ 代码已提交
- ✅ 单元测试通过
- ✅ 构建成功
- ✅ 容器已重启
- ✅ 可通过 https://10.211.55.5:8443/ 访问

## 总结

通过使用 **systematic-debugging** 技能，我们系统性地：

1. **追踪了数据流**：从营销网站 → localStorage → 控制台应用
2. **定位了根本原因**：两个应用使用不同的 localStorage 键名
3. **实施了最小修改**：只改变营销网站的键名，不影响控制台应用
4. **验证了修复**：单元测试 + 构建验证 + 部署验证

**结果**：用户现在可以在营销网站选择语言后，跳转到控制台应用时保持相同的语言设置。
