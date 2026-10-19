// sample —— 任务插件平台的最小参考插件。
//
// 用途（M6d Task 3）：跑通「编译 → T/D 加载期校验 → 注册 → registered 状态 →
// dryrun」全链。它不是 10 个内置平台之一，只用于测试与演示；内置平台源由
// 后续任务补齐（T3 报告登记为风险）。
//
// 形态冻结：经典脚本（非 ESM），执行后把导出对象赋给
// globalThis.__NEWTG_PLUGIN__（t3-brief 冻结约定 1）。插件只做数据变换，
// 不发请求、不碰文件/环境变量/异步（宿主拥有连接、持久化、轮询与计费）。
globalThis.__NEWTG_PLUGIN__ = {
  apiVersion: 1,
  key: "sample",
  name: "Sample",
  version: "1.0.0",
  author: { name: "NewTG" },
  description: {
    en: "Sample task plugin used by runtime tests",
    zh: "运行时测试用的示例任务插件",
  },
  models: ["sample-model"],
  fetchMode: "per_task",
  auth: { type: "api_key" },
  // 空数组/空对象显式写出：内置平台会填真实取值，这里保持最小声明。
  channelTypes: [],
  routes: [],
  protocols: {},
  // usageSchema 的数值字段是计费乘数，description 必须遵守 D 规则：
  // 英文以 unit price 结尾、中文含「单价」、无货币符号/数字/句末标点。
  usageSchema: {
    seconds: {
      type: "number",
      unit: "second",
      description: {
        en: "Video generation unit price",
        zh: "视频生成单价",
      },
    },
  },
  usageProfiles: [
    {
      models: ["sample-model"],
      schema: {
        seconds: {
          type: "number",
          unit: "second",
          description: {
            en: "Video generation unit price",
            zh: "视频生成单价",
          },
        },
      },
      examples: [{ seconds: 5 }],
    },
  ],
  usageExamples: [{ seconds: 5 }],
  requiredCapabilities: [],

  buildSubmitRequest: function (ctx) {
    return {
      url: (ctx.baseUrl || "https://example.invalid") + "/v1/sample/tasks",
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: {
        model: ctx.upstreamModel || ctx.model,
        prompt: ctx.prompt || "",
        seconds: ctx.seconds || 5,
      },
      responseType: "json",
    };
  },

  parseSubmitResponse: function (resp) {
    var body = {};
    try {
      body = JSON.parse(resp.body || "{}");
    } catch (e) {
      body = {};
    }
    return {
      taskId: String(body.id || "sample-task"),
      taskData: body,
      state: "SUBMITTED",
    };
  },

  buildQueryRequest: function (ctx) {
    return {
      url: (ctx.baseUrl || "https://example.invalid") + "/v1/sample/tasks/" + ctx.taskId,
      method: "GET",
    };
  },

  // 返回值词汇必须是契约 TaskState 7 态；未识别一律 UNKNOWN，
  // 不得默认成 IN_PROGRESS（relay-06 §3.7 / 契约 §2.5）。
  parseTaskResult: function (ctx, body) {
    var data = {};
    try {
      data = JSON.parse(body || "{}");
    } catch (e) {
      data = {};
    }
    if (data.status === "succeeded") {
      return { state: "SUCCESS" };
    }
    if (data.status === "failed") {
      return { state: "FAILURE" };
    }
    if (data.status === "running") {
      return { state: "IN_PROGRESS" };
    }
    return { state: "UNKNOWN" };
  },
};
