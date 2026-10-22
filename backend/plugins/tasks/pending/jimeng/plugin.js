// jimeng —— 即梦（Jimeng）视频任务插件占位源。
//
// 状态：显式 unsupported（builtinStatus = not_registered）。
//
// 仓库内已有的只是渠道契约，不是任务 API 契约：
//   - relay-02 / modules-基线-04：渠道类型 51（jimeng）默认 baseUrl
//     https://visual.volcengineapi.com，凭据为 Access Key ID / Secret
//     Access Key（火山签名）；
//   - relay-06 §1.1：视频（即梦），text_to_video / image_to_video /
//     first_tail_to_video，fetchMode=per_task，规范用量单位 second。
//
// 缺失的厂商契约（不可猜）：即梦视频任务的提交路径、请求体字段、火山 V4
// 签名所需的 service/region/action 参数、任务查询路径、状态词汇、产物字段
// 与用量字段。宿主当前只向插件注入 Authorization Bearer 形态的
// authHeader/apiKey，不足以表达 Ak/Sk 签名；因此生命周期钩子一律抛出
// unsupported，不拼接猜测 URL。
//
// models[] 里的 "jimeng" 是族级标识，真实模型 ID 由渠道 model_mapping 注入。
globalThis.__NEWTG_PLUGIN__ = {
  apiVersion: 1,
  key: "jimeng",
  name: "Jimeng",
  version: "0.1.0",
  author: { name: "NewTG" },
  description: {
    en: "Jimeng video task plugin (vendor contract pending)",
    zh: "即梦视频任务插件（厂商契约待补）",
  },
  models: ["jimeng"],
  fetchMode: "per_task",
  auth: { type: "api_key" },
  channelTypes: [51],
  upstreams: ["vendor", "new_api"],
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
      models: ["jimeng"],
      schema: {
        seconds: {
          type: "number",
          unit: "second",
          description: { en: "Video generation unit price", zh: "视频生成单价" },
        },
      },
      examples: [{ seconds: 5 }],
    },
  ],
  usageExamples: [{ seconds: 5 }],
  requiredCapabilities: [],

  builtinStatus: function () {
    return {
      status: "not_registered",
      missingContract:
        "Volcengine Visual Jimeng signed task submit/poll/artifact/usage contract (Ak/Sk signing parameters)",
    };
  },

  buildSubmitRequest: function () { return unsupported("buildSubmitRequest"); },
  parseSubmitResponse: function () { return unsupported("parseSubmitResponse"); },
  buildQueryRequest: function () { return unsupported("buildQueryRequest"); },
  parseTaskResult: function () { return unsupported("parseTaskResult"); },
};

function unsupported(hook) {
  throw new Error(
    "unsupported: jimeng vendor task contract is missing (" + hook + ")"
  );
}
