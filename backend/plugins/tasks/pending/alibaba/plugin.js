// alibaba —— Alibaba Bailian（Wan / Qwen-Image / Z-Image）任务插件占位源。
//
// 状态：显式 unsupported（builtinStatus = not_registered）。
//
// 仓库内已有的只是渠道契约，不是任务 API 契约：
//   - relay-02 / modules-基线-04：渠道类型 17（ali）默认 baseUrl
//     https://dashscope.aliyuncs.com，SingleKey（Bearer）；
//   - relay-06 §1.1 / modules-基线-08 §0.4：能力覆盖 Wan 视频与
//     Wan/Qwen-Image/Z-Image 图片，fetchMode=per_task。
//
// 缺失的厂商契约（不可猜）：DashScope 异步任务端点的提交路径、请求体字段、
// 任务查询路径、状态词汇、产物字段与用量字段。只有 baseUrl 和鉴权方式
// 不足以安全实现；因此生命周期钩子一律抛出 unsupported，绝不拼接猜测的
// URL。拿到 DashScope 任务 API 契约后再替换这些钩子即可注册。
//
// models[] 是族级标识（文档中的产品名），不是已验证的上游模型 ID；真实
// ID 由渠道 model_mapping 经 ctx.upstreamModel 注入。
globalThis.__NEWTG_PLUGIN__ = {
  apiVersion: 1,
  key: "alibaba",
  name: "Alibaba Bailian",
  version: "0.1.0",
  author: { name: "NewTG" },
  description: {
    en: "Alibaba Bailian video and image task plugin (vendor contract pending)",
    zh: "阿里百炼视频与图片任务插件（厂商契约待补）",
  },
  models: ["wan", "qwen-image", "z-image"],
  fetchMode: "per_task",
  auth: { type: "api_key" },
  channelTypes: [17],
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
      models: ["wan", "qwen-image", "z-image"],
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
        "DashScope/Wan asynchronous video and image task submit/poll/artifact/usage contract",
    };
  },

  buildSubmitRequest: function () { return unsupported("buildSubmitRequest"); },
  parseSubmitResponse: function () { return unsupported("parseSubmitResponse"); },
  buildQueryRequest: function () { return unsupported("buildQueryRequest"); },
  parseTaskResult: function () { return unsupported("parseTaskResult"); },
};

// unsupported 是显式失败：宿主会把它映射为插件钩子失败，绝不发出请求。
// 抛出而不是返回空描述符，避免「空 URL」被误当成可执行的提交。
function unsupported(hook) {
  throw new Error(
    "unsupported: alibaba vendor task contract is missing (" + hook + ")"
  );
}
