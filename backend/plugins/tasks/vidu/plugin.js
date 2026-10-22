// vidu —— Vidu 视频任务插件占位源。
//
// 状态：显式 unsupported（builtinStatus = not_registered）。
//
// 仓库内已有的只是渠道契约，不是任务 API 契约：
//   - relay-02 / modules-基线-04：渠道类型 52（vidu）默认 baseUrl
//     https://api.vidu.cn，SingleKey；
//   - relay-06 §1.1：视频（Vidu），text_to_video / image_to_video /
//     first_tail_to_video / reference_to_video，fetchMode=per_task，
//     规范用量单位 second 与 credit（厂商积分 credits，可小数）。
//
// 缺失的厂商契约（不可猜）：Vidu 视频任务的提交路径、请求体字段、任务查询
// 路径、状态词汇、产物字段、用量字段与 credits 语义。只有 baseUrl 与鉴权
// 方式不足以安全实现；因此生命周期钩子一律抛出 unsupported，不拼接猜测
// URL。
//
// models[] 里的 "vidu" 是族级标识，真实模型 ID 由渠道 model_mapping 注入。
globalThis.__NEWTG_PLUGIN__ = {
  apiVersion: 1,
  key: "vidu",
  name: "Vidu",
  version: "0.1.0",
  author: { name: "NewTG" },
  description: {
    en: "Vidu video task plugin (vendor contract pending)",
    zh: "Vidu 视频任务插件（厂商契约待补）",
  },
  models: ["vidu"],
  fetchMode: "per_task",
  auth: { type: "api_key" },
  channelTypes: [52],
  upstreams: ["vendor"],
  routes: [],
  protocols: {},
  usageSchema: {
    seconds: {
      type: "number",
      unit: "second",
      description: { en: "Video generation unit price", zh: "视频生成单价" },
    },
    credits: {
      type: "number",
      unit: "credit",
      description: { en: "Vidu credit unit price", zh: "Vidu 积分单价" },
    },
  },
  usageProfiles: [
    {
      models: ["vidu"],
      schema: {
        seconds: {
          type: "number",
          unit: "second",
          description: { en: "Video generation unit price", zh: "视频生成单价" },
        },
        credits: {
          type: "number",
          unit: "credit",
          description: { en: "Vidu credit unit price", zh: "Vidu 积分单价" },
        },
      },
      examples: [{ seconds: 5, credits: 4 }],
    },
  ],
  usageExamples: [{ seconds: 5, credits: 4 }],
  requiredCapabilities: [],

  builtinStatus: function () {
    return {
      status: "not_registered",
      missingContract:
        "Vidu video task submit/poll/artifact/usage contract and credits semantics",
    };
  },

  buildSubmitRequest: function () { return unsupported("buildSubmitRequest"); },
  parseSubmitResponse: function () { return unsupported("parseSubmitResponse"); },
  buildQueryRequest: function () { return unsupported("buildQueryRequest"); },
  parseTaskResult: function () { return unsupported("parseTaskResult"); },
};

function unsupported(hook) {
  throw new Error(
    "unsupported: vidu vendor task contract is missing (" + hook + ")"
  );
}
