// doubao —— Doubao（Seedance 视频 / Seedream 图片）任务插件占位源。
//
// 状态：显式 unsupported（builtinStatus = not_registered）。
//
// 仓库内已有的只是渠道契约，不是任务 API 契约：
//   - relay-02 / modules-基线-04：渠道类型 54（doubao_video）与 45
//     （volcengine，OpenAI 兼容 /api/v3）默认 baseUrl
//     https://ark.cn-beijing.volces.com，SingleKey（Bearer）；
//   - relay-06 §1.1：视频（Seedance）、图片（Seedream），fetchMode=per_task。
//
// 缺失的厂商契约（不可猜）：Ark 内容生成任务的提交路径、请求体字段、任务
// 查询路径、状态词汇、产物字段与用量字段。OpenAI 兼容只覆盖对话端点，
// 不能据此推断视频/图片任务端点。因此生命周期钩子一律抛出 unsupported，
// 不拼接猜测 URL，也不写任何占位端点。
//
// models[] 是族级标识（文档中的产品名），真实上游模型 ID 由渠道
// model_mapping 经 ctx.upstreamModel 注入。
globalThis.__NEWTG_PLUGIN__ = {
  apiVersion: 1,
  key: "doubao",
  name: "Doubao",
  version: "0.1.0",
  author: { name: "NewTG" },
  description: {
    en: "Doubao Seedance and Seedream task plugin (vendor contract pending)",
    zh: "豆包 Seedance 与 Seedream 任务插件（厂商契约待补）",
  },
  models: ["seedance", "seedream"],
  fetchMode: "per_task",
  auth: { type: "api_key" },
  channelTypes: [54, 45],
  upstreams: ["vendor", "new_api"],
  routes: [],
  protocols: {},
  usageSchema: {
    seconds: {
      type: "number",
      unit: "second",
      description: { en: "Video generation unit price", zh: "视频生成单价" },
    },
    image_count: {
      type: "number",
      unit: "count",
      description: { en: "Image generation unit price", zh: "图片生成单价" },
    },
    generate_audio: {
      type: "boolean",
      description: { en: "Generate audio", zh: "生成音频" },
    },
  },
  usageProfiles: [
    {
      models: ["seedance", "seedream"],
      schema: {
        seconds: {
          type: "number",
          unit: "second",
          description: { en: "Video generation unit price", zh: "视频生成单价" },
        },
        image_count: {
          type: "number",
          unit: "count",
          description: { en: "Image generation unit price", zh: "图片生成单价" },
        },
        generate_audio: {
          type: "boolean",
          description: { en: "Generate audio", zh: "生成音频" },
        },
      },
      examples: [{ seconds: 5 }, { image_count: 1 }],
    },
  ],
  usageExamples: [{ seconds: 5 }, { image_count: 1 }],
  requiredCapabilities: [],

  builtinStatus: function () {
    return {
      status: "not_registered",
      missingContract:
        "Volcengine Ark Seedance/Seedream content-generation task submit/poll/artifact/usage contract",
    };
  },

  buildSubmitRequest: function () { return unsupported("buildSubmitRequest"); },
  parseSubmitResponse: function () { return unsupported("parseSubmitResponse"); },
  buildQueryRequest: function () { return unsupported("buildQueryRequest"); },
  parseTaskResult: function () { return unsupported("parseTaskResult"); },
};

function unsupported(hook) {
  throw new Error(
    "unsupported: doubao vendor task contract is missing (" + hook + ")"
  );
}
