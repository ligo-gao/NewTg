// kling —— 可灵（Kling）视频任务插件占位源。
//
// 状态：显式 unsupported（builtinStatus = not_registered）。
//
// 仓库内已有的只是渠道契约，不是任务 API 契约：
//   - relay-02 / modules-基线-04：渠道类型 50（kling）默认 baseUrl
//     https://api.klingai.com，凭据为 AccessKey / SecretKey（上游为本网关
//     时退化为单 ApiKey）；
//   - relay-06 §1.1：视频（可灵），text_to_video / image_to_video，
//     fetchMode=per_task，规范用量单位 second 与 credit（厂商资源包 units，
//     可小数）。
//
// 缺失的厂商契约（不可猜）：可灵视频任务的提交路径、请求体字段、JWT/Ak-Sk
// 鉴权签名、任务查询路径、状态词汇、产物字段、用量字段与 units 语义。
// 只有 baseUrl 与凭据形态不足以安全实现；因此生命周期钩子一律抛出
// unsupported，不拼接猜测 URL。
//
// models[] 里的 "kling" 是族级标识，真实模型 ID 由渠道 model_mapping 注入。
globalThis.__NEWTG_PLUGIN__ = {
  apiVersion: 1,
  key: "kling",
  name: "Kling",
  version: "0.1.0",
  author: { name: "NewTG" },
  description: {
    en: "Kling video task plugin (vendor contract pending)",
    zh: "可灵视频任务插件（厂商契约待补）",
  },
  models: ["kling"],
  fetchMode: "per_task",
  auth: { type: "api_key" },
  channelTypes: [50],
  upstreams: ["vendor", "new_api"],
  routes: [],
  protocols: {},
  usageSchema: {
    seconds: {
      type: "number",
      unit: "second",
      description: { en: "Video generation unit price", zh: "视频生成单价" },
    },
    units: {
      type: "number",
      unit: "credit",
      description: { en: "Kling resource unit price", zh: "可灵资源单价" },
    },
  },
  usageProfiles: [
    {
      models: ["kling"],
      schema: {
        seconds: {
          type: "number",
          unit: "second",
          description: { en: "Video generation unit price", zh: "视频生成单价" },
        },
        units: {
          type: "number",
          unit: "credit",
          description: { en: "Kling resource unit price", zh: "可灵资源单价" },
        },
      },
      examples: [{ seconds: 5, units: 1 }],
    },
  ],
  usageExamples: [{ seconds: 5, units: 1 }],
  requiredCapabilities: [],

  builtinStatus: function () {
    return {
      status: "not_registered",
      missingContract:
        "Kling video task submit/poll/artifact/usage contract and AccessKey/SecretKey signing contract",
    };
  },

  buildSubmitRequest: function () { return unsupported("buildSubmitRequest"); },
  parseSubmitResponse: function () { return unsupported("parseSubmitResponse"); },
  buildQueryRequest: function () { return unsupported("buildQueryRequest"); },
  parseTaskResult: function () { return unsupported("parseTaskResult"); },
};

function unsupported(hook) {
  throw new Error(
    "unsupported: kling vendor task contract is missing (" + hook + ")"
  );
}
