// google —— Google Veo（Gemini API）任务插件占位源。
//
// 状态：显式 unsupported（builtinStatus = not_registered）。
//
// 仓库内已有的只是渠道契约，不是任务 API 契约：
//   - relay-02 / modules-基线-04：渠道类型 24（gemini）默认 baseUrl
//     https://generativelanguage.googleapis.com，单 key（Gemini 原生头）；
//   - relay-06 §1.1：视频（Veo），text_to_video / image_to_video，
//     fetchMode=per_task，规范用量单位 second 与 boolean（音频）。
//
// 缺失的厂商契约（不可猜）：Veo 长时操作（LRO）的提交路径、请求体字段、
// operation 查询路径、状态词汇、产物字段与用量字段；以及 Gemini 原生
// x-goog-api-key 头在插件上下文中的投影方式。只有 baseUrl 和鉴权类型
// 不足以安全实现，因此生命周期钩子一律抛出 unsupported。
//
// models[] 里的 "veo" 是族级标识，真实模型 ID 由渠道 model_mapping 注入。
globalThis.__NEWTG_PLUGIN__ = {
  apiVersion: 1,
  key: "google",
  name: "Google Veo",
  version: "0.1.0",
  author: { name: "NewTG" },
  description: {
    en: "Google Veo video task plugin for the Gemini API (vendor contract pending)",
    zh: "Google Veo 视频任务插件（Gemini API，厂商契约待补）",
  },
  models: ["veo"],
  fetchMode: "per_task",
  auth: { type: "api_key" },
  channelTypes: [24],
  upstreams: ["vendor"],
  routes: [],
  protocols: {},
  usageSchema: {
    seconds: {
      type: "number",
      unit: "second",
      description: { en: "Video generation unit price", zh: "视频生成单价" },
    },
    generate_audio: {
      type: "boolean",
      description: { en: "Generate audio", zh: "生成音频" },
    },
  },
  usageProfiles: [
    {
      models: ["veo"],
      schema: {
        seconds: {
          type: "number",
          unit: "second",
          description: { en: "Video generation unit price", zh: "视频生成单价" },
        },
        generate_audio: {
          type: "boolean",
          description: { en: "Generate audio", zh: "生成音频" },
        },
      },
      examples: [{ seconds: 8 }],
    },
  ],
  usageExamples: [{ seconds: 8 }],
  requiredCapabilities: [],

  builtinStatus: function () {
    return {
      status: "not_registered",
      missingContract:
        "Gemini API Veo long-running-operation submit/poll/artifact/usage contract and x-goog-api-key projection",
    };
  },

  buildSubmitRequest: function () { return unsupported("buildSubmitRequest"); },
  parseSubmitResponse: function () { return unsupported("parseSubmitResponse"); },
  buildQueryRequest: function () { return unsupported("buildQueryRequest"); },
  parseTaskResult: function () { return unsupported("parseTaskResult"); },
};

function unsupported(hook) {
  throw new Error(
    "unsupported: google vendor task contract is missing (" + hook + ")"
  );
}
