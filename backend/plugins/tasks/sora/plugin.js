// sora —— OpenAI Sora 视频任务插件。
//
// 实现依据（仓库内契约，不猜厂商字段）：
//   - relay-06 §3.8 / modules-基线-08 §9.1 冻结了 OpenAI Videos 形状：
//     POST /v1/videos、GET /v1/videos/{id}、GET /v1/videos/{id}/content；
//     这些形状被明确声明为「与公开 OpenAI API 形状一致」。
//   - relay-02 / modules-基线-04：渠道类型 55（sora）与 1（openai）的默认
//     baseUrl 是 https://api.openai.com，鉴权为 SingleKey（Bearer）。
//
// 因此本插件不硬编码厂商域名：请求 URL 一律是 ctx.baseUrl + 上述路径，
// 由渠道配置决定真实主机（新网关 new_api 上游也走同一路径）。
//
// models[] 里的 "sora" 是族级标识；真实上游模型 ID 由渠道 model_mapping
// 映射后经 ctx.upstreamModel 注入（缺省回退 ctx.model）。
//
// 插件只做数据变换：不发请求、不碰文件/环境变量/异步（宿主拥有连接、
// 持久化、轮询与计费）。状态词汇必须落在契约 7 态内；无法识别时返回
// UNKNOWN，不得默认成 IN_PROGRESS（relay-06 §3.7）。
globalThis.__NEWTG_PLUGIN__ = {
  apiVersion: 1,
  key: "sora",
  name: "Sora",
  version: "1.0.0",
  author: { name: "NewTG" },
  description: {
    en: "OpenAI Sora video task plugin using the OpenAI Videos API",
    zh: "基于 OpenAI Videos API 的 Sora 视频任务插件",
  },
  models: ["sora"],
  fetchMode: "per_task",
  auth: { type: "api_key" },
  channelTypes: [55, 1],
  upstreams: ["vendor", "new_api"],
  // 宿主协议声明：/v1/videos 只把请求交给声明 openai_video 的插件
  // （handlers_videos.go R-15）；decodeRequest/render 是加载期配对要求。
  protocols: { openai_video: {} },
  routes: [],
  // 秒是唯一的计费乘数；描述遵循 D1–D7（英文以 unit price 结尾、
  // 中文含单价、无数字/货币符号/句末标点）。
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
      models: ["sora"],
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
      examples: [{ seconds: 8 }],
    },
  ],
  usageExamples: [{ seconds: 8 }],
  requiredCapabilities: [],

  // builtinStatus 不是生命周期钩子，而是本仓库内置源的实现完备度自述：
  // 区分「按已冻结契约实现」（registered）与「仅占位、显式 unsupported」
  // （not_registered）。注册表徽章只表示源可编译且已装，不代表厂商契约
  // 已验证；验收以本声明为准（G-M6D-01）。
  builtinStatus: function () {
    return {
      status: "registered",
      contract:
        "OpenAI Videos API: POST /v1/videos, GET /v1/videos/{id}, GET /v1/videos/{id}/content (relay-06 §3.8)",
    };
  },

  // decodeRequest 是宿主协议（openai_video）要求的入站解码：OpenAI Videos
  // 的请求体字段（model/prompt/seconds/size）与 buildSubmitRequest 需要的
  // 输入同形，因此原样返回 ctx.body，不做字段改写。
  decodeRequest: function (ctx) {
    return (ctx && ctx.body) || {};
  },

  // render 是 openai_video 协议的成对钩子；当前 /v1/videos 由宿主内建薄封装
  // 渲染 video 对象，本钩子只满足加载期契约，不臆造额外字段。
  render: function () {
    return {};
  },

  buildSubmitRequest: function (ctx) {
    var input = (ctx && ctx.body) || {};
    var body = {
      model: (ctx && (ctx.upstreamModel || ctx.model)) || input.model,
    };
    var prompt = input.prompt;
    if (prompt === undefined && ctx) {
      prompt = ctx.prompt;
    }
    if (prompt !== undefined) {
      body.prompt = prompt;
    }
    var seconds = input.seconds;
    if (seconds === undefined && ctx) {
      seconds = ctx.seconds;
    }
    if (seconds !== undefined && seconds !== null && seconds !== "") {
      body.seconds = seconds;
    }
    var size = input.size;
    if (size !== undefined && size !== null && size !== "") {
      body.size = size;
    }
    return {
      url: (ctx && ctx.baseUrl) + "/v1/videos",
      method: "POST",
      headers: authHeaders(ctx, "application/json"),
      body: body,
      responseType: "json",
    };
  },

  parseSubmitResponse: function (resp) {
    var body = parseJSON(resp && resp.body);
    var id = body.id;
    if (id === undefined || id === null || id === "") {
      throw new Error("sora: submit response missing video id");
    }
    var state = mapStatus(body.status);
    var out = { taskId: String(id), taskData: body };
    if (state === "SUCCESS" || state === "FAILURE") {
      // 即时终态：落库即终态，不再轮询（relay-06 §6.2）。成功时把用量与
      // 产物清单写进 taskData，让提交期结算与产物投影与轮询终态同形
      // （宿主只从持久化 task.data 读取 usage/artifacts）。
      if (state === "SUCCESS") {
        var seconds = Number(body.seconds);
        if (isFinite(seconds) && seconds > 0) {
          body.usage = { seconds: seconds };
        }
        body.artifacts = [{ key: "video", type: "video", mimeType: "video/mp4" }];
      }
      out.immediate = { status: state };
    } else if (state !== "") {
      out.state = state;
    }
    return out;
  },

  buildQueryRequest: function (ctx) {
    return {
      url: (ctx && ctx.baseUrl) + "/v1/videos/" + encodeURIComponent(String(ctx.taskId)),
      method: "GET",
      headers: authHeaders(ctx, ""),
      responseType: "json",
    };
  },

  parseTaskResult: function (ctx, body) {
    var data = parseJSON(body);
    var state = mapStatus(data.status);
    if (state === "") {
      // 未识别一律 UNKNOWN（不得默认 IN_PROGRESS）。
      return { state: "UNKNOWN", taskData: data };
    }
    var out = { state: state, taskData: data };
    var progress = Number(data.progress);
    if (isFinite(progress)) {
      out.progress = progress;
    }
    if (state === "SUCCESS") {
      // 产物从持久化 task.data 投影：OpenAI Videos 的取内容端点是
      // /v1/videos/{id}/content，清单只需一个稳定 key。
      data.artifacts = [{ key: "video", type: "video", mimeType: "video/mp4" }];
      var seconds = Number(data.seconds);
      if (isFinite(seconds)) {
        out.usage = { seconds: seconds };
      }
    }
    if (state === "FAILURE" && data.error && data.error.message) {
      out.failReason = String(data.error.message);
    }
    return out;
  },

  // 提交预估：宿主在 tiered_expr 分支用它取请求侧秒数。
  extractUsage: function (ctx) {
    var input = (ctx && ctx.body) || {};
    var seconds = input.seconds;
    if (seconds === undefined && ctx) {
      seconds = ctx.seconds;
    }
    var n = Number(seconds);
    if (isFinite(n) && n > 0) {
      return { seconds: n };
    }
    return {};
  },

  // 即时结果：parseSubmitResponse 的 taskData 已含上游返回的 seconds。
  extractUsageOnSubmit: function (ctx, taskData) {
    return usageFromData(taskData);
  },

  // 完成结算：优先使用上游终态对象的 seconds。
  extractUsageOnComplete: function (ctx, result, data) {
    var usage = usageFromData(result && result.taskData);
    if (usage.seconds !== undefined) {
      return usage;
    }
    return usageFromData(data);
  },

  listArtifacts: function () {
    return [{ key: "video", type: "video", mimeType: "video/mp4" }];
  },

  buildContentRequest: function (ctx) {
    if (ctx && ctx.artifactKey && ctx.artifactKey !== "video") {
      throw new Error("sora: unknown artifact " + ctx.artifactKey);
    }
    return {
      url: (ctx && ctx.baseUrl) + "/v1/videos/" + encodeURIComponent(String(ctx.taskId)) + "/content",
      method: "GET",
      headers: authHeaders(ctx, ""),
    };
  },
};

// authHeaders 只使用宿主注入的凭据：优先 authHeader；旧路径没有 authHeader
// 时按 Bearer 约定拼 apiKey。绝不读取环境变量或自行签名。
function authHeaders(ctx, contentType) {
  var headers = {};
  if (contentType) {
    headers["Content-Type"] = contentType;
  }
  if (ctx && ctx.authHeader) {
    headers["Authorization"] = ctx.authHeader;
  } else if (ctx && ctx.apiKey) {
    headers["Authorization"] = "Bearer " + ctx.apiKey;
  }
  return headers;
}

// parseJSON 对坏 JSON 返回空对象，让调用方走 UNKNOWN/缺字段的显式分支，
// 而不是把异常吞成“成功”。
function parseJSON(raw) {
  if (raw && typeof raw === "object") {
    return raw;
  }
  try {
    return JSON.parse(raw || "{}");
  } catch (e) {
    return {};
  }
}

// mapStatus 把 OpenAI Videos 的状态词汇映射到契约 7 态；未知返回空串。
function mapStatus(raw) {
  var s = String(raw || "").toLowerCase();
  if (s === "completed" || s === "succeeded" || s === "success") {
    return "SUCCESS";
  }
  if (s === "failed" || s === "error" || s === "cancelled" || s === "canceled") {
    return "FAILURE";
  }
  if (s === "queued" || s === "queueing" || s === "queuing") {
    return "QUEUED";
  }
  if (s === "in_progress" || s === "inprogress" || s === "running" || s === "processing") {
    return "IN_PROGRESS";
  }
  if (s === "submitted") {
    return "SUBMITTED";
  }
  return "";
}

// usageFromData 只返回数值秒数（计费数量，不是价格）。
function usageFromData(data) {
  if (!data || typeof data !== "object") {
    return {};
  }
  var seconds = Number(data.seconds);
  if (isFinite(seconds) && seconds > 0) {
    return { seconds: seconds };
  }
  if (data.usage && typeof data.usage === "object") {
    var nested = Number(data.usage.seconds);
    if (isFinite(nested) && nested > 0) {
      return { seconds: nested };
    }
  }
  return {};
}
