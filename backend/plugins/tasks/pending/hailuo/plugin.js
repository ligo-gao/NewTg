// hailuo —— MiniMax 海螺视频任务插件占位源。
//
// 状态：显式 unsupported（builtinStatus = not_registered）。
//
// 仓库内已有的只是渠道契约，不是任务 API 契约：
//   - relay-02 / modules-基线-04：渠道类型 35（minimax）默认 baseUrl
//     https://api.minimax.chat，SingleKey；
//   - relay-06 §1.1：视频（MiniMax 海螺，H3 多模态参考），
//     text_to_video / image_to_video / reference_to_video，
//     fetchMode=per_task，规范用量单位 second。
//
// 缺失的厂商契约（不可猜）：海螺视频任务的提交路径、请求体字段、任务查询
// 路径、状态词汇、产物字段与用量字段。MiniMax 自有协议（非 OpenAI 兼容）
// 不能从现有对话适配器推出视频任务形状；因此生命周期钩子一律抛出
// unsupported，不拼接猜测 URL。
//
// models[] 里的 "hailuo" 是族级标识，真实模型 ID 由渠道 model_mapping 注入。
globalThis.__NEWTG_PLUGIN__ = {
  apiVersion: 1,
  key: "hailuo",
  name: "Hailuo Video",
  version: "0.1.0",
  author: { name: "NewTG" },
  description: {
    en: "MiniMax Hailuo video task plugin (vendor contract pending)",
    zh: "MiniMax 海螺视频任务插件（厂商契约待补）",
  },
  models: ["hailuo"],
  fetchMode: "per_task",
  auth: { type: "api_key" },
  channelTypes: [35],
  upstreams: ["vendor"],
  routes: [],
  protocols: {},
  usageSchema: {
    seconds: {
      type: "number",
      unit: "second",
      description: { en: "Video generation unit price", zh: "视频生成单价" },
    },
  },
  usageProfiles: [
    {
      models: ["hailuo"],
      schema: {
        seconds: {
          type: "number",
          unit: "second",
          description: { en: "Video generation unit price", zh: "视频生成单价" },
        },
      },
      examples: [{ seconds: 6 }],
    },
  ],
  usageExamples: [{ seconds: 6 }],
  requiredCapabilities: [],

  builtinStatus: function () {
    return {
      status: "not_registered",
      missingContract:
        "MiniMax Hailuo video task submit/poll/artifact/usage contract",
    };
  },

  buildSubmitRequest: function () { return unsupported("buildSubmitRequest"); },
  parseSubmitResponse: function () { return unsupported("parseSubmitResponse"); },
  buildQueryRequest: function () { return unsupported("buildQueryRequest"); },
  parseTaskResult: function () { return unsupported("parseTaskResult"); },
};

function unsupported(hook) {
  throw new Error(
    "unsupported: hailuo vendor task contract is missing (" + hook + ")"
  );
}
