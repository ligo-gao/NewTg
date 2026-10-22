// sunoapi —— SunoAPI 音乐/歌词任务插件占位源（fetchMode = batch）。
//
// 状态：显式 unsupported（builtinStatus = not_registered）。
//
// 仓库内已有的只是渠道契约，不是任务 API 契约：
//   - relay-02 / modules-基线-04：渠道类型 36（sunoapi）默认 baseUrl 必填，
//     SingleKey；
//   - relay-06 §1.1：音乐 / 歌词，动作 music / lyrics，**fetchMode=batch**，
//     规范用量单位 count（clips 条数）与 enum（action）。
//
// 缺失的厂商契约（不可猜）：SunoAPI 的提交路径（按 action 分支）、请求体
// 字段、批量拉取任务的路径与批次形状、任务状态词汇、产物字段与 clips
// 语义。只有 baseUrl 与鉴权方式不足以安全实现；因此全部生命周期钩子
// 抛出 unsupported，不拼接猜测 URL。
//
// 批次模式要求 buildBatchQueryRequest + parseBatchResult（relay-06 §3.6），
// 占位源同样导出它们并显式失败，避免加载期 hook_missing 把「契约缺失」
// 伪装成「源写坏了」。
//
// models[] 里的 "suno" 是族级标识，真实模型 ID 由渠道 model_mapping 注入。
globalThis.__NEWTG_PLUGIN__ = {
  apiVersion: 1,
  key: "sunoapi",
  name: "SunoAPI",
  version: "0.1.0",
  author: { name: "NewTG" },
  description: {
    en: "SunoAPI music and lyrics task plugin (vendor contract pending)",
    zh: "SunoAPI 音乐与歌词任务插件（厂商契约待补）",
  },
  models: ["suno"],
  fetchMode: "batch",
  auth: { type: "api_key" },
  channelTypes: [36],
  upstreams: ["vendor", "new_api"],
  routes: [],
  protocols: {},
  usageSchema: {
    clips: {
      type: "number",
      unit: "count",
      description: {
        en: "Music clip generation unit price",
        zh: "音乐片段生成单价",
      },
    },
    action: {
      type: "enum",
      enum: ["music", "lyrics"],
      enumLabels: {
        music: { en: "Music generation", zh: "音乐生成" },
        lyrics: { en: "Lyrics generation", zh: "歌词生成" },
      },
      description: { en: "Generation action", zh: "生成动作" },
    },
  },
  usageProfiles: [
    {
      models: ["suno"],
      schema: {
        clips: {
          type: "number",
          unit: "count",
          description: {
            en: "Music clip generation unit price",
            zh: "音乐片段生成单价",
          },
        },
        action: {
          type: "enum",
          enum: ["music", "lyrics"],
          enumLabels: {
            music: { en: "Music generation", zh: "音乐生成" },
            lyrics: { en: "Lyrics generation", zh: "歌词生成" },
          },
          description: { en: "Generation action", zh: "生成动作" },
        },
      },
      examples: [{ clips: 1, action: "music" }],
    },
  ],
  usageExamples: [{ clips: 1, action: "music" }],
  requiredCapabilities: [],

  builtinStatus: function () {
    return {
      status: "not_registered",
      missingContract:
        "SunoAPI music/lyrics submit, batch-fetch, artifact and usage contract",
    };
  },

  buildSubmitRequest: function () { return unsupported("buildSubmitRequest"); },
  parseSubmitResponse: function () { return unsupported("parseSubmitResponse"); },
  parseTaskResult: function () { return unsupported("parseTaskResult"); },
  buildBatchQueryRequest: function () { return unsupported("buildBatchQueryRequest"); },
  parseBatchResult: function () { return unsupported("parseBatchResult"); },
};

function unsupported(hook) {
  throw new Error(
    "unsupported: sunoapi vendor task contract is missing (" + hook + ")"
  );
}
